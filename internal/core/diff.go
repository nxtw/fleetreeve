package core

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Diff returns a unified diff of two texts with three lines of context.
func Diff(from, to string) string {
	if !utf8.ValidString(from) || !utf8.ValidString(to) {
		return "(binary content differs)\n"
	}
	a, b := splitLines(from), splitLines(to)
	n, m := len(a), len(b)
	if n*m > 4_000_000 {
		return fmt.Sprintf("(%d lines -> %d lines; too large to diff)\n", n, m)
	}
	// Longest common subsequence table.
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	type op struct {
		kind   byte
		text   string
		al, bl int // 1-based line numbers at this op
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i + 1, j + 1})
			i, j = i+1, j+1
		case j >= m || (i < n && dp[i+1][j] >= dp[i][j+1]):
			ops = append(ops, op{'-', a[i], i + 1, j + 1})
			i++
		default:
			ops = append(ops, op{'+', b[j], i + 1, j + 1})
			j++
		}
	}
	const ctx = 3
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != ' ' {
			for x := max(0, k-ctx); x <= min(len(ops)-1, k+ctx); x++ {
				keep[x] = true
			}
		}
	}
	var sb strings.Builder
	for k := 0; k < len(ops); {
		if !keep[k] {
			k++
			continue
		}
		end := k
		ac, bc := 0, 0
		for end < len(ops) && keep[end] {
			if ops[end].kind != '+' {
				ac++
			}
			if ops[end].kind != '-' {
				bc++
			}
			end++
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", ops[k].al, ac, ops[k].bl, bc)
		for ; k < end; k++ {
			sb.WriteByte(ops[k].kind)
			sb.WriteString(ops[k].text)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
