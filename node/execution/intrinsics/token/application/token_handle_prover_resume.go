package application

import (
	"encoding/binary"

	"github.com/pkg/errors"
	"source.quilibrium.com/quilibrium/monorepo/node/protobufs"
)

func (a *TokenApplication) handleDataAnnounceProverResume(
	currentFrameNumber uint64,
	lockMap map[string]struct{},
	t *protobufs.AnnounceProverResume,
) (
	[]*protobufs.TokenOutput,
	error,
) {
	if currentFrameNumber < PROOF_FRAME_CUTOFF {
		return nil, errors.Wrap(ErrInvalidStateTransition, "handle resume")
	}

	payload := []byte("resume")

	if t == nil || t.PublicKeySignatureEd448 == nil {
		return nil, errors.Wrap(ErrInvalidStateTransition, "handle resume")
	}

	if t.PublicKeySignatureEd448.PublicKey == nil ||
		t.PublicKeySignatureEd448.Signature == nil ||
		t.PublicKeySignatureEd448.PublicKey.KeyValue == nil ||
		t.Filter == nil || len(t.Filter) != 32 ||
		t.FrameNumber > currentFrameNumber {
		return nil, errors.Wrap(ErrInvalidStateTransition, "handle resume")
	}

	if _, touched := lockMap[string(
		t.PublicKeySignatureEd448.PublicKey.KeyValue,
	)]; touched {
		return nil, errors.Wrap(ErrInvalidStateTransition, "handle resume")
	}

	payload = binary.BigEndian.AppendUint64(payload, t.FrameNumber)
	payload = append(payload, t.Filter...)

	if err := t.PublicKeySignatureEd448.Verify(payload); err != nil {
		return nil, errors.Wrap(ErrInvalidStateTransition, "handle resume")
	}

	address, err := a.getAddressFromSignature(t.PublicKeySignatureEd448)
	if err != nil {
		return nil, errors.Wrap(err, "handle resume")
	}

	inTries := false
	for _, t := range a.Tries {
		inTries = inTries || t.Contains(address)
	}

	lockMap[string(t.PublicKeySignatureEd448.PublicKey.KeyValue)] = struct{}{}
	if !inTries {
		return nil, errors.Wrap(errors.New("in prover trie"), "handle resume")
	}

	return []*protobufs.TokenOutput{
		&protobufs.TokenOutput{
			Output: &protobufs.TokenOutput_Resume{
				Resume: t,
			},
		},
	}, nil
}
