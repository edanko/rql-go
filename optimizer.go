package rql

// This file holds the only AST rewrites the translator performs. Every rewrite
// is semantics preserving; rewrites that change results are deliberately absent:
//
//   - repeated equality is never collapsed into IN or a containment test
//     (a=1&a=2 must stay two conjuncts, otherwise AND silently becomes OR or the
//     query breaks on non-array columns);
//   - equality is never rewritten into IN.
//
// Flattening is performed while the tree is built (see addKid), and negation is
// normalised here.

// addKid appends child to parent, flattening a child of the same conjunction
// kind: and(a, and(b, c)) becomes and(a, b, c). Flattening changes no result and
// keeps the rendered SQL flat.
func (p *parser) addKid(parent, child astIndex) {
	if child == noNode {
		return
	}
	// Read through the arena by index: addKid itself never appends nodes, but the
	// copy keeps this safe if that ever changes.
	parentKind := p.nodes[parent].kind
	if childNode := p.nodes[child]; childNode.kind == parentKind && (parentKind == astAnd || parentKind == astOr) {
		count := childNode.kidCount()
		for i := 0; i < count; i++ {
			p.nodes[parent].addKid(childNode.kidAt(i))
		}
		return
	}
	p.nodes[parent].addKid(child)
}

// negate builds the negation of node, applying double-negation elimination and
// De Morgan's laws:
//
//	not(not(A))        -> A
//	not(and(A, B, C))  -> or(not(A), not(B), not(C))
//	not(or(A, B, C))   -> and(not(A), not(B), not(C))
//
// Negation of a predicate is left as NOT (predicate) so that SQL's three-valued
// logic is preserved exactly.
func (p *parser) negate(node astIndex, pos int) astIndex {
	// Copy the node: negate may append to the arena and grow it.
	n := p.nodes[node]
	switch n.kind {
	case astNot:
		// not(not(A)) -> A
		return n.kidAt(0)
	case astAnd, astOr:
		flipped := astAnd
		if n.kind == astAnd {
			flipped = astOr
		}
		out := p.addNode(astNode{kind: flipped, pos: pos})
		count := n.kidCount()
		for i := 0; i < count; i++ {
			p.addKid(out, p.negate(n.kidAt(i), pos))
		}
		return out
	default:
		out := p.addNode(astNode{kind: astNot, pos: pos})
		p.addKid(out, node)
		return out
	}
}

// addNode appends a node to the arena and returns its index.
func (p *parser) addNode(n astNode) astIndex {
	p.nodes = append(p.nodes, n)
	return astIndex(len(p.nodes) - 1)
}
