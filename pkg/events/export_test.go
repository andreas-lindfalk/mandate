package events

import (
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func exportedChain(t *testing.T, tenantId uuid.UUID, n int) []Exported {
	t.Helper()
	prev := genesisHash
	run := "run-1"
	events := make([]Exported, 0, n)
	createdAt := time.Date(2026, 11, 24, 9, 0, 0, 123000, time.UTC)
	for i := 1; i <= n; i++ {
		payload, err := canonicalJson(map[string]any{"n": i, "action": fmt.Sprintf("tickets.create%d", i)})
		require.NoError(t, err)
		stored := Stored{Seq: int64(i), Type: "proposal.created", ActorKind: "agent", RunId: &run, Payload: payload, CreatedAt: createdAt}
		hash, err := chainHash(tenantId, prev, stored)
		require.NoError(t, err)
		events = append(events, Exported{
			Seq: stored.Seq, Type: stored.Type, ActorKind: stored.ActorKind, RunId: &run, Payload: payload,
			CreatedAt: createdAt.Format(time.RFC3339Nano), PrevHash: hex.EncodeToString(prev), Hash: hex.EncodeToString(hash),
		})
		prev = hash
	}
	return events
}

func TestVerifyExportedAcceptsAnIntactChain(t *testing.T) {
	tenantId := uuid.New()
	events := exportedChain(t, tenantId, 4)
	head, err := VerifyExported(tenantId, events)
	require.NoError(t, err)
	require.Equal(t, events[3].Hash, head)

	head, err = VerifyExported(tenantId, nil)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(genesisHash), head, "an empty chain is the genesis hash")
}

func TestVerifyExportedNamesTheFirstBrokenEvent(t *testing.T) {
	tenantId := uuid.New()
	other := "run-2"
	for name, test := range map[string]struct {
		change func(events []Exported) []Exported
		seq    int64
	}{
		"seq gap":    {func(events []Exported) []Exported { return append(events[:1], events[2:]...) }, 3},
		"renumbered": {func(events []Exported) []Exported { events[1].Seq = 5; return events }, 5},
		"payload edited": {func(events []Exported) []Exported {
			events[2].Payload = []byte(`{"action":"tickets.create3","n":30}`)
			return events
		}, 3},
		"run id edited": {func(events []Exported) []Exported { events[1].RunId = &other; return events }, 2},
		"actor edited":  {func(events []Exported) []Exported { events[0].ActorKind = "person"; return events }, 1},
		"removed last":  {func(events []Exported) []Exported { return events[:3] }, 0},
	} {
		t.Run(name, func(t *testing.T) {
			events := test.change(exportedChain(t, tenantId, 4))
			head, err := VerifyExported(tenantId, events)
			if test.seq == 0 {
				require.NoError(t, err, "a shortened chain verifies on its own; the head hash shows what is missing")
				require.NotEqual(t, exportedChain(t, tenantId, 4)[3].Hash, head)
				return
			}
			require.ErrorIs(t, err, ErrBroken)
			require.ErrorContains(t, err, fmt.Sprintf("at seq %d", test.seq))
		})
	}
}

func TestVerifyExportedRefusesAnotherTenantsChain(t *testing.T) {
	events := exportedChain(t, uuid.New(), 2)
	_, err := VerifyExported(uuid.New(), events)
	require.ErrorIs(t, err, ErrBroken)
}
