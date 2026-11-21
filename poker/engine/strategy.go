package engine

// Strategy gives action probabilities at an infoset: (decision node,
// bucket of the acting player). out has n.NumActions entries.
type Strategy interface {
	Probs(n *Node, bucket int, out []float64)
}

// UniformStrategy plays every action equally often: the untrained
// starting point, and a fixed target for best-response tests.
type UniformStrategy struct{}

func (UniformStrategy) Probs(n *Node, _ int, out []float64) {
	for a := range out {
		out[a] = 1 / float64(n.NumActions)
	}
}

// AlwaysCall checks or calls at every decision: the calling station. With
// AlwaysFold it is a control whose best response can be worked out by
// hand, for checking the tools that measure exploitability.
type AlwaysCall struct{ Tree *Tree }

func (s AlwaysCall) Probs(n *Node, _ int, out []float64) {
	clear(out)
	for a := range out {
		if k := s.Tree.Actions[int(n.FirstChild)+a].Kind; k == ActCheck || k == ActCall {
			out[a] = 1
			return
		}
	}
	out[0] = 1 // a node with neither (not produced by the tree builders)
}

// AlwaysFold folds whenever it can and checks otherwise.
type AlwaysFold struct{ Tree *Tree }

func (s AlwaysFold) Probs(n *Node, _ int, out []float64) {
	clear(out)
	pick := -1
	for a := range out {
		switch s.Tree.Actions[int(n.FirstChild)+a].Kind {
		case ActFold:
			out[a] = 1
			return
		case ActCheck:
			pick = a
		}
	}
	if pick < 0 {
		pick = 0
	}
	out[pick] = 1
}

// PreflopRaiseScaled plays another strategy with every bet or raise of
// the first round, the all-in included, made M times as likely and each
// infoset renormalised. Later rounds are the other strategy's unchanged.
// M = 1 is the other strategy.
type PreflopRaiseScaled struct {
	Of   Strategy
	Tree *Tree
	M    float64
}

func (s PreflopRaiseScaled) Probs(n *Node, bucket int, out []float64) {
	s.Of.Probs(n, bucket, out)
	if n.Round != 0 {
		return
	}
	var total float64
	for a := range out {
		if s.Tree.Actions[int(n.FirstChild)+a].Kind == ActBet {
			out[a] *= s.M
		}
		total += out[a]
	}
	if total > 0 {
		for a := range out {
			out[a] /= total
		}
	}
}
