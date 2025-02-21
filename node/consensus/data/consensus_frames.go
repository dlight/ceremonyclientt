package data

import (
	"bytes"
	"context"
	"time"

	"golang.org/x/crypto/sha3"
	"source.quilibrium.com/quilibrium/monorepo/node/config"
	"source.quilibrium.com/quilibrium/monorepo/node/consensus"
	"source.quilibrium.com/quilibrium/monorepo/node/consensus/data/internal"
	"source.quilibrium.com/quilibrium/monorepo/node/internal/frametime"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"source.quilibrium.com/quilibrium/monorepo/node/execution/intrinsics/token/application"
	"source.quilibrium.com/quilibrium/monorepo/node/protobufs"
)

const defaultSyncTimeout = 4 * time.Second

func (e *DataClockConsensusEngine) collect(
	enqueuedFrame *protobufs.ClockFrame,
) (*protobufs.ClockFrame, error) {
	e.logger.Info("collecting vdf proofs")

	latest := enqueuedFrame
	for {
		candidates := e.GetAheadPeers(max(latest.FrameNumber, e.latestFrameReceived))
		if len(candidates) == 0 {
			break
		}
		for _, candidate := range candidates {
			if candidate.MaxFrame <= max(latest.FrameNumber, e.latestFrameReceived) {
				continue
			}
			var err error
			latest, err = e.sync(latest, candidate.MaxFrame, candidate.PeerID)
			if err != nil {
				e.logger.Debug("error syncing frame", zap.Error(err))
				continue
			}
		}
	}

	e.logger.Info(
		"returning leader frame",
		zap.Uint64("frame_number", latest.FrameNumber),
		zap.Duration("frame_age", frametime.Since(latest)),
	)

	return latest, nil
}

func (e *DataClockConsensusEngine) prove(
	previousFrame *protobufs.ClockFrame,
) (*protobufs.ClockFrame, error) {
	if e.lastProven >= previousFrame.FrameNumber && e.lastProven != 0 {
		return previousFrame, nil
	}
	e.stagedTransactionsMx.Lock()
	executionOutput := &protobufs.IntrinsicExecutionOutput{}
	_, tries, err := e.clockStore.GetDataClockFrame(
		e.filter,
		previousFrame.FrameNumber,
		false,
	)
	app, err := application.MaterializeApplicationFromFrame(
		e.provingKey,
		previousFrame,
		tries,
		e.coinStore,
		e.clockStore,
		e.pubSub,
		e.logger,
	)
	if err != nil {
		e.stagedTransactionsMx.Unlock()
		return nil, errors.Wrap(err, "prove")
	}

	if e.stagedTransactions == nil {
		e.stagedTransactions = &protobufs.TokenRequests{}
	}

	e.logger.Info(
		"proving new frame",
		zap.Int("transactions", len(e.stagedTransactions.Requests)),
	)

	var validTransactions *protobufs.TokenRequests
	var invalidTransactions *protobufs.TokenRequests
	app, validTransactions, invalidTransactions, err = app.ApplyTransitions(
		previousFrame.FrameNumber+1,
		e.stagedTransactions,
		true,
	)
	if err != nil {
		e.stagedTransactions = &protobufs.TokenRequests{}
		e.stagedTransactionsMx.Unlock()
		return nil, errors.Wrap(err, "prove")
	}

	e.logger.Info(
		"applied transitions",
		zap.Int("successful", len(validTransactions.Requests)),
		zap.Int("failed", len(invalidTransactions.Requests)),
	)
	e.stagedTransactions = &protobufs.TokenRequests{}
	e.stagedTransactionsMx.Unlock()

	outputState, err := app.MaterializeStateFromApplication()
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	executionOutput.Address = application.TOKEN_ADDRESS
	executionOutput.Output, err = proto.Marshal(outputState)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	executionOutput.Proof, err = proto.Marshal(validTransactions)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	data, err := proto.Marshal(executionOutput)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	e.logger.Debug("encoded execution output")
	digest := sha3.NewShake256()
	_, err = digest.Write(data)
	if err != nil {
		e.logger.Error(
			"error writing digest",
			zap.Error(err),
		)
		return nil, errors.Wrap(err, "prove")
	}

	expand := make([]byte, 1024)
	_, err = digest.Read(expand)
	if err != nil {
		e.logger.Error(
			"error expanding digest",
			zap.Error(err),
		)
		return nil, errors.Wrap(err, "prove")
	}

	commitment, err := e.inclusionProver.CommitRaw(
		expand,
		16,
	)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	e.logger.Debug("creating kzg proof")
	proof, err := e.inclusionProver.ProveRaw(
		expand,
		int(expand[0]%16),
		16,
	)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	e.logger.Debug("finalizing execution proof")

	frame, err := e.frameProver.ProveDataClockFrame(
		previousFrame,
		[][]byte{proof},
		[]*protobufs.InclusionAggregateProof{
			{
				Filter:      e.filter,
				FrameNumber: previousFrame.FrameNumber + 1,
				InclusionCommitments: []*protobufs.InclusionCommitment{
					{
						Filter:      e.filter,
						FrameNumber: previousFrame.FrameNumber + 1,
						TypeUrl:     protobufs.IntrinsicExecutionOutputType,
						Commitment:  commitment,
						Data:        data,
						Position:    0,
					},
				},
				Proof: proof,
			},
		},
		e.provingKey,
		time.Now().UnixMilli(),
		e.difficulty,
	)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	e.lastProven = previousFrame.FrameNumber
	e.logger.Info(
		"returning new proven frame",
		zap.Uint64("frame_number", frame.FrameNumber),
		zap.Int("proof_count", len(frame.AggregateProofs)),
		zap.Int("commitment_count", len(frame.Input[516:])/74),
	)
	return frame, nil
}

func (e *DataClockConsensusEngine) GetAheadPeers(frameNumber uint64) []internal.PeerCandidate {
	if e.GetFrameProverTries()[0].Contains(e.provingKeyAddress) {
		return nil
	}

	e.logger.Debug(
		"checking peer list",
		zap.Int("peers", len(e.peerMap)),
		zap.Int("uncooperative_peers", len(e.uncooperativePeersMap)),
		zap.Uint64("current_head_frame", frameNumber),
	)

	candidates := make([]internal.WeightedPeerCandidate, 0, len(e.peerMap))
	maxDiff := uint64(0)

	e.peerMapMx.RLock()
	for _, v := range e.peerMap {
		e.logger.Debug(
			"checking peer info",
			zap.Binary("peer_id", v.peerId),
			zap.Uint64("max_frame_number", v.maxFrame),
			zap.Int64("timestamp", v.timestamp),
			zap.Binary("version", v.version),
		)
		if v.maxFrame <= frameNumber {
			continue
		}
		if _, ok := e.uncooperativePeersMap[string(v.peerId)]; ok {
			continue
		}
		if v.timestamp <= config.GetMinimumVersionCutoff().UnixMilli() {
			continue
		}
		if bytes.Compare(v.version, config.GetMinimumVersion()) < 0 {
			continue
		}
		maxDiff = max(maxDiff, v.maxFrame-frameNumber)
		candidates = append(candidates, internal.WeightedPeerCandidate{
			PeerCandidate: internal.PeerCandidate{
				PeerID:   v.peerId,
				MaxFrame: v.maxFrame,
			},
		})
	}
	e.peerMapMx.RUnlock()

	if len(candidates) == 0 {
		return nil
	}

	for i := range candidates {
		candidates[i].Weight = float64(candidates[i].MaxFrame-frameNumber) / float64(maxDiff)
	}
	return internal.WeightedSampleWithoutReplacement(candidates, len(candidates))
}

func (e *DataClockConsensusEngine) sync(
	currentLatest *protobufs.ClockFrame,
	maxFrame uint64,
	peerId []byte,
) (*protobufs.ClockFrame, error) {
	e.syncingStatus = SyncStatusSynchronizing
	defer func() { e.syncingStatus = SyncStatusNotSyncing }()
	latest := currentLatest
	e.logger.Info(
		"polling peer for new frames",
		zap.String("peer_id", peer.ID(peerId).String()),
		zap.Uint64("current_frame", latest.FrameNumber),
		zap.Uint64("max_frame", maxFrame),
	)
	var cooperative bool = true
	defer func() {
		if cooperative {
			return
		}
		e.peerMapMx.Lock()
		defer e.peerMapMx.Unlock()
		if _, ok := e.peerMap[string(peerId)]; ok {
			e.uncooperativePeersMap[string(peerId)] = e.peerMap[string(peerId)]
			e.uncooperativePeersMap[string(peerId)].timestamp = time.Now().UnixMilli()
			delete(e.peerMap, string(peerId))
		}
	}()
	cc, err := e.pubSub.GetDirectChannel(peerId, "sync")
	if err != nil {
		e.logger.Debug(
			"could not establish direct channel",
			zap.Error(err),
		)
		cooperative = false
		return latest, errors.Wrap(err, "sync")
	}
	defer func() {
		if err := cc.Close(); err != nil {
			e.logger.Error("error while closing connection", zap.Error(err))
		}
	}()

	client := protobufs.NewDataServiceClient(cc)

	syncTimeout := e.config.Engine.SyncTimeout
	if syncTimeout == 0 {
		syncTimeout = defaultSyncTimeout
	}

	for e.GetState() < consensus.EngineStateStopping {
		ctx, cancel := context.WithTimeout(e.ctx, syncTimeout)
		response, err := client.GetDataFrame(
			ctx,
			&protobufs.GetDataFrameRequest{
				FrameNumber: latest.FrameNumber + 1,
			},
			grpc.MaxCallRecvMsgSize(600*1024*1024),
		)
		cancel()
		if err != nil {
			e.logger.Debug(
				"could not get frame",
				zap.Error(err),
			)
			cooperative = false
			return latest, errors.Wrap(err, "sync")
		}

		if response == nil {
			e.logger.Debug("received no response from peer")
			return latest, nil
		}

		if response.ClockFrame == nil ||
			response.ClockFrame.FrameNumber != latest.FrameNumber+1 ||
			response.ClockFrame.Timestamp < latest.Timestamp {
			e.logger.Debug("received invalid response from peer")
			cooperative = false
			return latest, nil
		}
		e.logger.Info(
			"received new leading frame",
			zap.Uint64("frame_number", response.ClockFrame.FrameNumber),
			zap.Duration("frame_age", frametime.Since(response.ClockFrame)),
		)
		if !e.IsInProverTrie(
			response.ClockFrame.GetPublicKeySignatureEd448().PublicKey.KeyValue,
		) {
			cooperative = false
		}
		if err := e.frameProver.VerifyDataClockFrame(
			response.ClockFrame,
		); err != nil {
			return nil, errors.Wrap(err, "sync")
		}
		e.dataTimeReel.Insert(response.ClockFrame, true)
		latest = response.ClockFrame
		if latest.FrameNumber >= maxFrame {
			return latest, nil
		}
	}
	return latest, nil
}
