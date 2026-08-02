package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Two audiences read this CLI's output: a person, and a script. A table with
// aligned columns is right for the first and unparseable for the second, so
// commands describe what they are reporting and the printer renders it for
// whichever is asking.
//
// The alternative — every command formatting twice — is how the table and the
// JSON drift apart.

// printer renders command results.
type printer struct {
	w    io.Writer
	json bool
	// quiet reduces output to the identifiers a script would pipe onward,
	// which is otherwise a job for awk on a table that may be reformatted.
	quiet bool
}

func newPrinter(w io.Writer, flags map[string]string) *printer {
	return &printer{
		w:     w,
		json:  flags["json"] == "true",
		quiet: flags["quiet"] == "true" || flags["q"] == "true",
	}
}

// row is one record: ordered columns for a person, keyed fields for a script.
type row struct {
	// id is what --quiet prints, and is normally the first column too.
	id     string
	fields []field
}

type field struct {
	name  string
	value any
}

func (r row) with(name string, value any) row {
	r.fields = append(r.fields, field{name, value})
	return r
}

func newRow(idName, id string) row {
	return row{id: id, fields: []field{{idName, id}}}
}

func (r row) toMap() map[string]any {
	m := make(map[string]any, len(r.fields))
	for _, f := range r.fields {
		m[f.name] = f.value
	}
	return m
}

// table prints rows under a heading derived from the first row's field names.
func (p *printer) table(key string, rows []row) error {
	if p.json {
		items := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			items = append(items, r.toMap())
		}
		return p.encode(map[string]any{key: items})
	}
	if p.quiet {
		for _, r := range rows {
			if _, err := fmt.Fprintln(p.w, r.id); err != nil {
				return err
			}
		}
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	tw := tabwriter.NewWriter(p.w, 2, 4, 2, ' ', 0)
	heads := make([]string, 0, len(rows[0].fields))
	for _, f := range rows[0].fields {
		heads = append(heads, strings.ToUpper(columnName(f.name)))
	}
	fmt.Fprintln(tw, strings.Join(heads, "\t"))
	for _, r := range rows {
		cells := make([]string, 0, len(r.fields))
		for _, f := range r.fields {
			cells = append(cells, fmt.Sprint(f.value))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// record prints a single object: one line per field rather than a wide table,
// since a detail view has more fields than a terminal has columns.
func (p *printer) record(r row) error {
	if p.json {
		return p.encode(r.toMap())
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.w, r.id)
		return err
	}
	tw := tabwriter.NewWriter(p.w, 2, 4, 2, ' ', 0)
	for _, f := range r.fields {
		fmt.Fprintf(tw, "%s:\t%v\n", f.name, f.value)
	}
	return tw.Flush()
}

// note is progress or advice for a person: suppressed by --quiet and absent
// from --json, where it would be noise in a parsed result.
func (p *printer) note(format string, args ...any) {
	if p.json || p.quiet {
		return
	}
	fmt.Fprintf(p.w, format+"\n", args...)
}

// result reports an action that produced no record beyond its own success.
func (p *printer) result(id string, fields ...field) error {
	r := row{id: id}
	if id != "" {
		r.fields = append(r.fields, field{"id", id})
	}
	r.fields = append(r.fields, fields...)
	if p.json {
		return p.encode(r.toMap())
	}
	if p.quiet {
		if id == "" {
			return nil
		}
		_, err := fmt.Fprintln(p.w, id)
		return err
	}
	parts := make([]string, 0, len(fields)+1)
	if id != "" {
		parts = append(parts, id)
	}
	for _, f := range fields {
		parts = append(parts, fmt.Sprint(f.value))
	}
	_, err := fmt.Fprintln(p.w, strings.Join(parts, "\t"))
	return err
}

func (p *printer) encode(v any) error {
	enc := json.NewEncoder(p.w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// columnName turns a camelCase field name into spaced words for a heading, so
// the JSON key and the column title come from one string.
func columnName(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sortedLabels renders a label map deterministically. Map iteration order would
// otherwise make output differ between runs, which breaks both diffing and tests.
func sortedLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	out := make([]string, 0, len(labels))
	for k, v := range labels {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// readyState reports whether an image can be used without a pull. The count is
// how many machines hold it, which is not something a caller can act on.
func readyState(copies int) string {
	if copies > 0 {
		return "ready"
	}
	return "warming"
}

// orEmptyMap keeps a nil label map from encoding as JSON null, which a caller
// would have to special-case before indexing it.
func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
