package scenario

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Expand turns a matrix into one file per combination of its values (names in sorted
// order, the last varying fastest). In each, ${name} is replaced in string values, never
// in keys, and Matrix is cleared and MatrixRow says which combination it is. A file with
// no matrix is returned as it is.
func Expand(f *File) ([]*File, error) {
	if len(f.Matrix) == 0 {
		return []*File{f}, nil
	}
	if f.raw == nil {
		return nil, errors.New("scenario: a matrix can only be expanded from a parsed file")
	}
	names := sortedKeys(f.Matrix)
	rows := []map[string]string{{}}
	for _, n := range names {
		if len(f.Matrix[n]) == 0 {
			return nil, fmt.Errorf("scenario: matrix %q has no values", n)
		}
		var next []map[string]string
		for _, row := range rows {
			for _, val := range f.Matrix[n] {
				r := map[string]string{n: val}
				for k, v := range row {
					r[k] = v
				}
				next = append(next, r)
			}
		}
		rows = next
	}
	out := make([]*File, 0, len(rows))
	for _, row := range rows {
		doc := substitute(f.raw, row)
		for i := 0; i+1 < len(doc.Content); i += 2 {
			if doc.Content[i].Value == "matrix" {
				doc.Content = append(doc.Content[:i], doc.Content[i+2:]...)
				break
			}
		}
		g, err := decodeDoc(doc)
		if err != nil {
			return nil, err
		}
		g.MatrixRow = row
		out = append(out, g)
	}
	return out, nil
}

// substitute copies the tree with ${name} replaced in scalar values. Mapping keys are
// the even-indexed children and stay as written.
func substitute(n *yaml.Node, row map[string]string) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, ch := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			k := *ch
			c.Content[i] = &k
			continue
		}
		c.Content[i] = substitute(ch, row)
	}
	if n.Kind == yaml.ScalarNode {
		for name, val := range row {
			c.Value = strings.ReplaceAll(c.Value, "${"+name+"}", val)
		}
	}
	return &c
}
