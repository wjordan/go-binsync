package release

// PlanKind is what a target must fetch to reach the head release.
type PlanKind int

const (
	// PlanNone means there is nothing to fetch: either the target already
	// holds the head (Plan.AtHead) or nothing published reaches it.
	PlanNone PlanKind = iota
	PlanChain
	PlanBlob
)

func (k PlanKind) String() string {
	switch k {
	case PlanChain:
		return "chain"
	case PlanBlob:
		return "blob"
	}
	return "none"
}

// Plan is the work one target must do to become the head release.
type Plan struct {
	Kind  PlanKind
	Edges []Edge // oldest first, ready to apply in order; nil unless PlanChain
	Blob  *Blob  // nil unless PlanBlob
	Bytes int64  // total bytes this plan will fetch
	Head  Hash

	atHead bool
}

// AtHead separates the two PlanNone outcomes: true means the target is
// already the head release, false means no published object reaches it and
// the caller has nothing to try (README exit code 4).
func (p Plan) AtHead() bool { return p.atHead }

// MakePlan decides how a target holding current reaches p's head
// (docs/DESIGN.md 4.2). It prefers the patch chain, which is why the chain
// exists, and falls back to the blob whenever the chain does not reach
// current or would cost more bytes than the whole binary.
func MakePlan(p *Pointer, current Hash) Plan {
	head := p.Head.Hash
	if current == head {
		return Plan{Kind: PlanNone, Head: head, atHead: true}
	}

	// Chain is newest-first, so the suffix p.Chain[:k+1] whose oldest edge
	// starts at current is the path; only one edge can match, because the
	// chain is a linked list of distinct releases.
	for k := 0; k < len(p.Chain) && k < MaxChain; k++ {
		if p.Chain[k].From != current {
			continue
		}
		var bytes int64
		for _, e := range p.Chain[:k+1] {
			bytes += e.Size
		}
		if p.Head.Blob != nil && bytes >= p.Head.Blob.Size {
			break
		}
		edges := make([]Edge, k+1)
		for j, e := range p.Chain[:k+1] {
			edges[k-j] = e
		}
		return Plan{Kind: PlanChain, Edges: edges, Bytes: bytes, Head: head}
	}

	if p.Head.Blob != nil {
		return Plan{Kind: PlanBlob, Blob: p.Head.Blob, Bytes: p.Head.Blob.Size, Head: head}
	}
	return Plan{Kind: PlanNone, Head: head}
}
