package compiler

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kyleconroy/sqlc/internal/metadata"
	"github.com/kyleconroy/sqlc/internal/source"
	"github.com/kyleconroy/sqlc/internal/sql/ast"
	"github.com/kyleconroy/sqlc/internal/sql/ast/pg"
	"github.com/kyleconroy/sqlc/internal/sql/astutils"
	"github.com/kyleconroy/sqlc/internal/sql/catalog"
	"github.com/kyleconroy/sqlc/internal/sql/rewrite"
	"github.com/kyleconroy/sqlc/internal/sql/validate"
)

var ErrUnsupportedStatementType = errors.New("parseQuery: unsupported statement type")

func rewriteNumberedParameters(refs []paramRef, raw *ast.RawStmt, sql string) ([]source.Edit, error) {
	edits := make([]source.Edit, len(refs))
	for i, ref := range refs {
		edits[i] = source.Edit{
			Location: ref.ref.Location - raw.StmtLocation,
			Old:      fmt.Sprintf("$%d", ref.ref.Number),
			New:      "?",
		}
	}
	return edits, nil
}

func parseQuery(p Parser, c *catalog.Catalog, stmt ast.Node, src string, rewriteParameters bool) (*Query, error) {
	if err := validate.ParamStyle(stmt); err != nil {
		return nil, err
	}
	if err := validate.ParamRef(stmt); err != nil {
		return nil, err
	}
	raw, ok := stmt.(*ast.RawStmt)
	if !ok {
		return nil, errors.New("node is not a statement")
	}
	switch n := raw.Stmt.(type) {
	case *pg.SelectStmt:
	case *pg.DeleteStmt:
	case *pg.InsertStmt:
		if err := validate.InsertStmt(n); err != nil {
			return nil, err
		}
	case *pg.TruncateStmt:
	case *pg.UpdateStmt:
	default:
		return nil, ErrUnsupportedStatementType
	}

	rawSQL, err := source.Pluck(src, raw.StmtLocation, raw.StmtLen)
	if err != nil {
		return nil, err
	}
	if rawSQL == "" {
		return nil, errors.New("missing semicolon at end of file")
	}
	if err := validate.FuncCall(c, raw); err != nil {
		return nil, err
	}
	name, cmd, err := metadata.Parse(strings.TrimSpace(rawSQL), p.CommentSyntax())
	if err != nil {
		return nil, err
	}
	if err := validate.Cmd(raw.Stmt, name, cmd); err != nil {
		return nil, err
	}

	raw, namedParams, edits := rewrite.NamedParameters(raw)
	rvs := rangeVars(raw.Stmt)
	refs := findParameters(raw.Stmt)
	if rewriteParameters {
		edits, err = rewriteNumberedParameters(refs, raw, rawSQL)
		if err != nil {
			return nil, err
		}
	} else {
		refs = uniqueParamRefs(refs)
		sort.Slice(refs, func(i, j int) bool { return refs[i].ref.Number < refs[j].ref.Number })
	}
	params, err := resolveCatalogRefs(c, rvs, refs, namedParams)
	if err != nil {
		return nil, err
	}

	qc, err := buildQueryCatalog(c, raw.Stmt)
	if err != nil {
		return nil, err
	}
	cols, err := outputColumns(qc, raw.Stmt)
	if err != nil {
		return nil, err
	}

	expandEdits, err := expand(qc, raw)
	if err != nil {
		return nil, err
	}
	edits = append(edits, expandEdits...)

	expanded, err := source.Mutate(rawSQL, edits)
	if err != nil {
		return nil, err
	}

	// If the query string was edited, make sure the syntax is valid
	if expanded != rawSQL {
		if _, err := p.Parse(strings.NewReader(expanded)); err != nil {
			return nil, fmt.Errorf("edited query syntax is invalid: %w", err)
		}
	}

	trimmed, comments, err := source.StripComments(expanded)
	if err != nil {
		return nil, err
	}

	return &Query{
		Cmd:      cmd,
		Comments: comments,
		Name:     name,
		Params:   params,
		Columns:  cols,
		SQL:      trimmed,
	}, nil
}

func rangeVars(root ast.Node) []*pg.RangeVar {
	var vars []*pg.RangeVar
	find := astutils.VisitorFunc(func(node ast.Node) {
		switch n := node.(type) {
		case *pg.RangeVar:
			vars = append(vars, n)
		}
	})
	astutils.Walk(find, root)
	return vars
}

func uniqueParamRefs(in []paramRef) []paramRef {
	m := make(map[int]struct{}, len(in))
	o := make([]paramRef, 0, len(in))
	for _, v := range in {
		if _, ok := m[v.ref.Number]; !ok {
			m[v.ref.Number] = struct{}{}
			o = append(o, v)
		}
	}
	return o
}
