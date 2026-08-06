package store

import (
	"strconv"
	"strings"
	"testing"
)

// The placeholder rewrite is the one piece of the dialect layer that can be wrong
// silently. A miscount does not fail to compile and does not fail to run -- it binds an
// argument to the wrong column, so a reservation is recorded against another node or a
// state check compares the wrong field.
func TestPostgresBindNumbersPlaceholdersInOrder(t *testing.T) {
	d := postgresDialect{}
	for _, tc := range []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"WHERE id=?", "WHERE id=$1"},
		{"WHERE a=? AND b=?", "WHERE a=$1 AND b=$2"},
		{"VALUES(?,?,?)", "VALUES($1,$2,$3)"},
		// Ten or more, because a naive implementation that formats a single digit
		// breaks here and every statement in this package with 10+ arguments would
		// bind wrongly.
		{"VALUES(?,?,?,?,?,?,?,?,?,?,?)", "VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)"},
	} {
		if got := d.bind(tc.in); got != tc.want {
			t.Errorf("bind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPostgresBindLeavesQuotedQuestionMarksAlone(t *testing.T) {
	// No statement in this package contains a `?` inside a literal today. That is
	// precisely the kind of fact that stops being true without anyone noticing, and the
	// symptom would be a rewritten literal -- a WHERE clause comparing against "$1"
	// rather than against a question mark.
	d := postgresDialect{}
	for _, tc := range []struct{ in, want string }{
		{"WHERE note='what?' AND id=?", "WHERE note='what?' AND id=$1"},
		{`WHERE "odd?col" = ?`, `WHERE "odd?col" = $1`},
		// An escaped quote inside a literal must not be read as the literal ending,
		// which would leave the parser inverted for the rest of the statement.
		{"WHERE a='it''s ?' AND b=?", "WHERE a='it''s ?' AND b=$1"},
	} {
		if got := d.bind(tc.in); got != tc.want {
			t.Errorf("bind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSQLiteBindIsIdentity(t *testing.T) {
	// SQLite takes `?` as written, so the rewrite must be a no-op rather than a
	// transformation that happens to round-trip.
	d := sqliteDialect{}
	q := "INSERT INTO t(a,b) VALUES(?,?) ON CONFLICT(a) DO UPDATE SET b=excluded.b"
	if got := d.bind(q); got != q {
		t.Errorf("sqlite bind altered the query:\n got %q\nwant %q", got, q)
	}
}

// TestEveryStatementBindsTheSameNumberOfPlaceholders guards the property that matters
// across the whole package rather than in one statement: however many `?` a query has,
// the rewrite must produce exactly that many distinct `$n`, numbered from 1 with no
// gaps. A gap or a repeat is an argument bound to the wrong column.
func TestEveryStatementBindsTheSameNumberOfPlaceholders(t *testing.T) {
	d := postgresDialect{}
	for _, q := range []string{
		`UPDATE snapshots SET ref_count = ref_count + 1 WHERE id = ? AND state = ?`,
		`DELETE FROM snapshots WHERE id = ? AND ref_count = 0
		   AND NOT EXISTS (SELECT 1 FROM snapshots c WHERE c.base_id = ?)`,
		`INSERT INTO nodes(id, region, labels, runtimes, cpu_alloc, mem_alloc,
		   disk_alloc, gpu_count, max_creates, cached_images, nvme_cache, state,
		   advertise_addr, last_heartbeat, cpu_vendor, cpu_family, cpu_template)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
	} {
		want := strings.Count(q, "?")
		got := d.bind(q)

		// Extracted rather than counted with strings.Count, and that is not fussiness:
		// counting "$1" also matches inside "$10".."$17", so the obvious version of this
		// test reported "$1 appears 9 times" against a rewrite that was correct. A test
		// that cries wolf on correct code gets loosened, and then it stops catching the
		// real thing.
		nums := extractPlaceholders(got)
		if len(nums) != want {
			t.Errorf("query has %d `?` but %d placeholders came out:\n%s",
				want, len(nums), got)
			continue
		}
		for i, n := range nums {
			if n != i+1 {
				t.Errorf("placeholder %d of %d is $%d, want $%d -- an out-of-order or "+
					"repeated number binds an argument to the wrong column:\n%s",
					i+1, want, n, i+1, got)
				break
			}
		}
		if strings.Contains(got, "?") {
			t.Errorf("a placeholder survived the rewrite:\n%s", got)
		}
	}
}

// extractPlaceholders returns the numbers of every $n in order.
func extractPlaceholders(q string) []int {
	var out []int
	for i := 0; i < len(q); i++ {
		if q[i] != '$' {
			continue
		}
		j := i + 1
		for j < len(q) && q[j] >= '0' && q[j] <= '9' {
			j++
		}
		if j == i+1 {
			continue
		}
		n, err := strconv.Atoi(q[i+1 : j])
		if err != nil {
			continue
		}
		out = append(out, n)
		i = j - 1
	}
	return out
}
