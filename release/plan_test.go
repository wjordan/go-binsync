package release

import (
	"reflect"
	"testing"
)

func th(n byte) Hash {
	var h Hash
	h[0], h[31] = n, n
	return h
}

// chain builds a newest-first chain of n edges ending at th(n+1), each of
// size bytes: th(1)->th(2), ..., th(n)->th(n+1).
func chain(n int, size int64) []Edge {
	var out []Edge
	for i := n; i >= 1; i-- {
		from, to := th(byte(i)), th(byte(i+1))
		out = append(out, Edge{From: from, To: to, Key: PatchKey(from, to), Size: size})
	}
	return out
}

func ptr(head Hash, blobSize int64, edges []Edge) *Pointer {
	p := &Pointer{Format: Format, Seq: 1, Head: Release{Hash: head, Size: 1 << 20}, Chain: edges}
	if blobSize > 0 {
		p.Head.Blob = &Blob{Key: BlobKey(head), Size: blobSize}
	}
	return p
}

func TestMakePlan(t *testing.T) {
	t.Parallel()

	// A chain of 4 edges of 100 B each ending at th(5), blob of 10000 B.
	full := ptr(th(5), 10000, chain(4, 100))

	tests := []struct {
		name    string
		p       *Pointer
		current Hash
		kind    PlanKind
		atHead  bool
		bytes   int64
		edges   []Hash // From of each planned edge, in apply order
	}{
		{name: "at head", p: full, current: th(5), kind: PlanNone, atHead: true},
		{name: "one behind", p: full, current: th(4), kind: PlanChain, bytes: 100,
			edges: []Hash{th(4)}},
		{name: "three behind", p: full, current: th(2), kind: PlanChain, bytes: 300,
			edges: []Hash{th(2), th(3), th(4)}},
		{name: "oldest on chain", p: full, current: th(1), kind: PlanChain, bytes: 400,
			edges: []Hash{th(1), th(2), th(3), th(4)}},
		{name: "drifted", p: full, current: th(99), kind: PlanBlob, bytes: 10000},
		{name: "no current file", p: full, current: Hash{}, kind: PlanBlob, bytes: 10000},
		{name: "chain reaches but costs more than the blob",
			p: ptr(th(5), 250, chain(4, 100)), current: th(2), kind: PlanBlob, bytes: 250},
		{name: "chain costs exactly the blob", // strictly cheaper or take the blob
			p: ptr(th(5), 300, chain(4, 100)), current: th(2), kind: PlanBlob, bytes: 300},
		{name: "no blob, chain reaches", p: ptr(th(5), 0, chain(4, 100)), current: th(3),
			kind: PlanChain, bytes: 200, edges: []Hash{th(3), th(4)}},
		{name: "no blob, chain does not reach", p: ptr(th(5), 0, chain(4, 100)),
			current: th(99), kind: PlanNone},
		{name: "no blob, no chain", p: ptr(th(5), 0, nil), current: th(99), kind: PlanNone},
		{name: "empty chain, drifted", p: ptr(th(5), 10000, nil), current: th(99),
			kind: PlanBlob, bytes: 10000},
		{name: "further back than MaxChain", // edge MaxChain+1 is never used
			p: ptr(th(byte(MaxChain+2)), 10000, chain(MaxChain+1, 100)), current: th(1),
			kind: PlanBlob, bytes: 10000},
		{name: "exactly MaxChain behind",
			p: ptr(th(byte(MaxChain+2)), 10000, chain(MaxChain+1, 100)), current: th(2),
			kind: PlanChain, bytes: 100 * MaxChain},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MakePlan(tc.p, tc.current)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v", got.Kind, tc.kind)
			}
			if got.AtHead() != tc.atHead {
				t.Errorf("AtHead = %v, want %v", got.AtHead(), tc.atHead)
			}
			if got.Bytes != tc.bytes {
				t.Errorf("Bytes = %d, want %d", got.Bytes, tc.bytes)
			}
			if got.Head != tc.p.Head.Hash {
				t.Errorf("Head = %v, want %v", got.Head, tc.p.Head.Hash)
			}
			if (got.Blob != nil) != (tc.kind == PlanBlob) {
				t.Errorf("Blob = %v for kind %v", got.Blob, tc.kind)
			}
			if tc.edges != nil {
				var from []Hash
				for _, e := range got.Edges {
					from = append(from, e.From)
				}
				if !reflect.DeepEqual(from, tc.edges) {
					t.Errorf("edge From = %v, want %v", from, tc.edges)
				}
			}
			if tc.kind != PlanChain && got.Edges != nil {
				t.Errorf("Edges = %v for kind %v", got.Edges, tc.kind)
			}
		})
	}
}

// The planned edges must chain from the target's current hash to the head,
// in the order the caller applies them.
func TestMakePlanEdgesApplyInOrder(t *testing.T) {
	t.Parallel()
	p := ptr(th(5), 10000, chain(4, 100))
	plan := MakePlan(p, th(1))
	if plan.Kind != PlanChain {
		t.Fatalf("kind = %v", plan.Kind)
	}
	at := th(1)
	for i, e := range plan.Edges {
		if e.From != at {
			t.Fatalf("edge %d starts at %v, target is at %v", i, e.From, at)
		}
		at = e.To
	}
	if at != p.Head.Hash {
		t.Fatalf("chain ends at %v, head is %v", at, p.Head.Hash)
	}
}

func TestPlanKindString(t *testing.T) {
	t.Parallel()
	for k, want := range map[PlanKind]string{PlanNone: "none", PlanChain: "chain", PlanBlob: "blob", 99: "none"} {
		if got := k.String(); got != want {
			t.Errorf("PlanKind(%d) = %q, want %q", int(k), got, want)
		}
	}
}
