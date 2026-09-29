package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Record lists under pressure: keep records whole, not hollow.
//
// Prod 2026-09-29 (conv 61411, the owner's test number): a customer asked for
// the price of full-face Botox and Fillmed cheek filler. Every catalog lookup
// returned the right data — sections → services → packages, each package with
// its price and the branch it is sold in (Amman 180, Irbid 144 on the summer
// offer; filler 200 / 160) — and every one reached the model as a list of
// services whose packages lists read ["[4 entries dropped …]"]. The fitter's
// first structural step was to halve the deepest lists, on the theory that a
// list nested inside a record is that record's history. In a catalog the nested
// list is the thing on offer. Left with service names and no prices, the model
// quoted a price its persona uses as an example (144, the Irbid offer) as the
// Amman price, told the customer prices were the same in every branch, and
// sent her to the clinic for the filler price; asked for the other services it
// made twelve lookups in three and a half minutes and quoted none.
//
// Nothing in the shape tells a catalog from a history: the same tool family
// nests 1–3 past reservations inside each of a customer's packages exactly the
// way it nests 4 priced packages inside each service. What does hold for every
// payload is that a record whose nested lists were emptied can no longer be
// quoted at all, while a record that is still whole can. The same failure has
// now surfaced three times — slot times (v0.25), offer packages (v0.27), service
// packages (here) — each time as a hollowed record. So:
//
//  1. Lossless first. A list of flat records — objects whose values are all
//     plain scalars — becomes one table: the field names once, one row of values
//     per record, and the fields that hold a single value in every record stated
//     once beside it (agyTabulate). Nothing is lost; the repeated field names go,
//     and a catalog's price lists shrink by roughly a third. A record that an
//     older result shares byte for byte with a newer one is kept in the newer
//     one only (agyDedupeRepeatedRecords).
//  2. Complete over hollow. When a list of records still has to shrink, its
//     trailing records give up their nested lists and long text first, keeping
//     their own short facts (stubTrailingRecords), while the leading records stay
//     whole. Only once a single whole record is left, and only from the prose
//     level on — after every instruction sheet has been shortened — are lists
//     thinned: first the reduced records of a nested list (dropTrailingStubs),
//     then the lists inside the last whole record. A result's own record list
//     keeps every record, reduced or not, until the last level.
//
// Like the rest of the fitter this knows nothing about any API: which records
// lead is the order the API returned them in, and what a record keeps when it
// is reduced is decided by size and type alone.

// Table keys. Plain words, because the model reads them.
const (
	agyTableColumns = "columns"
	agyTableRows    = "rows"
	agyTableSame    = "same_in_every_row"
)

// agyStubKey marks a record that gave up its nested lists and long text.
const agyStubKey = "_details_dropped_to_fit"

// agyRecordNoteKeys are the fields the fitter itself writes into records. They
// say what was done to the record and are never spent as data.
var agyRecordNoteKeys = []string{agyDroppedFieldsKey, agyStubKey, agyRepeatedKey}

func agyIsRecordNoteKey(k string) bool {
	for _, note := range agyRecordNoteKeys {
		if k == note {
			return true
		}
	}
	return false
}

// agyStubScalarBytes is the longest scalar (as JSON) a reduced record keeps:
// ids, names, prices, dates, counts and statuses fit; descriptions do not.
const agyStubScalarBytes = 64

// A table is only how a list is written into the prompt. The fitter works on
// records — its prose, column, stub and thinning passes all read lists of
// objects — so a result is decoded back into records (agyUntabulate) each time
// it is read, and written as tables (agyTabulated) each time it is measured or
// kept. Nothing about the fitter's choices depends on the encoding.

// agyTableOf builds the table for a list of flat records — objects whose values
// are all plain scalars — or reports false when the list is not one or the
// table would not be smaller. A field some records lack reads null in their
// rows: catalogs carry offer fields only on the packages that belong to an
// offer, and to a reader "no offer" is what both mean. The notes the fitter
// writes about the list itself — the columns it dropped (on the first record)
// and the entries it dropped (a marker after the last) — stay with the table.
func agyTableOf(list []any) (map[string]any, bool) {
	records := list
	var omitted any
	if n := len(records); n > 0 {
		if s, ok := records[n-1].(string); ok && strings.HasPrefix(s, agyOmissionMarker) {
			omitted, records = s, records[:n-1]
		}
	}
	if len(records) < 2 {
		return nil, false
	}
	var droppedFields any
	seen := map[string]bool{}
	var fields []string
	for i, item := range records {
		obj, ok := item.(map[string]any)
		if !ok || len(obj) == 0 {
			return nil, false
		}
		for k, val := range obj {
			switch val.(type) {
			case map[string]any, []any:
				return nil, false
			}
			if k == agyDroppedFieldsKey {
				if i > 0 {
					return nil, false
				}
				droppedFields = val
				continue
			}
			if !seen[k] {
				seen[k] = true
				fields = append(fields, k)
			}
		}
	}
	sort.Strings(fields)

	same := map[string]any{}
	var columns []string
	for _, f := range fields {
		first, err := marshalJSONNoHTML(records[0].(map[string]any)[f])
		if err != nil {
			return nil, false
		}
		constant := true
		for _, item := range records[1:] {
			raw, err := marshalJSONNoHTML(item.(map[string]any)[f])
			if err != nil {
				return nil, false
			}
			if string(raw) != string(first) {
				constant = false
				break
			}
		}
		if constant {
			same[f] = records[0].(map[string]any)[f]
		} else {
			columns = append(columns, f)
		}
	}
	if len(columns) == 0 {
		return nil, false // identical records: nothing to lay out
	}
	rows := make([]any, 0, len(list))
	for _, item := range records {
		obj := item.(map[string]any)
		row := make([]any, len(columns))
		for i, c := range columns {
			row[i] = obj[c]
		}
		rows = append(rows, row)
	}
	if omitted != nil {
		rows = append(rows, omitted)
	}
	colVals := make([]any, len(columns))
	for i, c := range columns {
		colVals[i] = c
	}
	table := map[string]any{agyTableColumns: colVals, agyTableRows: rows}
	if len(same) > 0 {
		table[agyTableSame] = same
	}
	if droppedFields != nil {
		table[agyDroppedFieldsKey] = droppedFields
	}
	before, err1 := marshalJSONNoHTML(list)
	after, err2 := marshalJSONNoHTML(table)
	if err1 != nil || err2 != nil || len(after) >= len(before) {
		return nil, false
	}
	return table, true
}

// agyTabulated returns a copy of v with every list of flat records written as
// a table, and reports whether it wrote any. v itself is not changed.
func agyTabulated(v any) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		changed := false
		for k, item := range x {
			nv, c := agyTabulated(item)
			out[k] = nv
			changed = changed || c
		}
		return out, changed
	case []any:
		out := make([]any, len(x))
		changed := false
		for i, item := range x {
			nv, c := agyTabulated(item)
			out[i] = nv
			changed = changed || c
		}
		if table, ok := agyTableOf(out); ok {
			return table, true
		}
		return out, changed
	}
	return v, false
}

// agyAsTable reports whether m is written as a table: its header, its rows —
// each exactly as long as the header, or the omission marker after the last —
// and nothing but the keys a table carries.
func agyAsTable(m map[string]any) ([]string, []any, bool) {
	cols, ok := m[agyTableColumns].([]any)
	rows, ok2 := m[agyTableRows].([]any)
	if !ok || !ok2 || len(cols) == 0 {
		return nil, nil, false
	}
	for k := range m {
		switch k {
		case agyTableColumns, agyTableRows, agyTableSame, agyDroppedFieldsKey:
		default:
			return nil, nil, false
		}
	}
	if same, present := m[agyTableSame]; present {
		if _, ok := same.(map[string]any); !ok {
			return nil, nil, false
		}
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		s, ok := c.(string)
		if !ok {
			return nil, nil, false
		}
		names[i] = s
	}
	for i, row := range rows {
		if s, ok := row.(string); ok && i == len(rows)-1 && strings.HasPrefix(s, agyOmissionMarker) {
			continue
		}
		cells, ok := row.([]any)
		if !ok || len(cells) != len(names) {
			return nil, nil, false
		}
	}
	return names, rows, true
}

// agyUntabulate rewrites, anywhere inside v, every table back into its list of
// records. A null cell is a field the record did not carry.
func agyUntabulate(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if names, rows, ok := agyAsTable(x); ok {
			same, _ := x[agyTableSame].(map[string]any)
			list := make([]any, 0, len(rows))
			for _, row := range rows {
				cells, ok := row.([]any)
				if !ok {
					list = append(list, row) // the omission marker
					continue
				}
				obj := make(map[string]any, len(names)+len(same))
				for k, val := range same {
					obj[k] = val
				}
				for i, name := range names {
					if cells[i] != nil {
						obj[name] = cells[i]
					}
				}
				list = append(list, obj)
			}
			if note, present := x[agyDroppedFieldsKey]; present && len(list) > 0 {
				if obj, ok := list[0].(map[string]any); ok {
					obj[agyDroppedFieldsKey] = note
				}
			}
			return list
		}
		for k, item := range x {
			x[k] = agyUntabulate(item)
		}
		return x
	case []any:
		for i := range x {
			x[i] = agyUntabulate(x[i])
		}
		return x
	}
	return v
}

// Repeated records. The same turn fetched the catalog nine times over — sections
// "2,3,6,8,13,17,19,23,25", then "2,3", "19,23", "6,8", "13,25", then "3", "2",
// "19", "23" — because each copy was dropped to fit and the note said to call
// again. Every copy then competed for the same bytes. A stale copy used to mean
// the same tool asked the same question (agySupersededResults); a result that
// asked a wider question is not that, but each record in it that a later result
// carries byte for byte is. Those records are replaced by a pointer that keeps
// their own short facts. Nothing is lost: the record is in the later result.

// agyRepeatedRecordMinBytes is the smallest record worth replacing by a pointer.
const agyRepeatedRecordMinBytes = 200

// agyRepeatedKey marks a record whose full copy is in a later result.
const agyRepeatedKey = "_shown_in_full_in_a_later_result"

// agyDedupeRepeatedRecords returns, per tool-result turn index, the text with
// every record that a later result of the same tool carries replaced by a
// pointer — only for results it changed.
func agyDedupeRepeatedRecords(turns []agyTurn) map[int]string {
	later := map[string]map[string]bool{} // tool → records seen in later results
	out := map[int]string{}
	for i := len(turns) - 1; i >= 0; i-- {
		tt := turns[i]
		if tt.Kind != agyTurnToolResult || tt.Tool == "" {
			continue
		}
		v, ok := agyDecodeJSONValue(tt.Text)
		if !ok {
			continue
		}
		if seen := later[tt.Tool]; len(seen) > 0 {
			if nv, changed := agyPointRepeatedRecords(v, seen); changed {
				if raw, err := marshalJSONNoHTML(nv); err == nil && len(raw) < len(tt.Text) {
					out[i] = string(raw)
				}
			}
		}
		// What this result holds is available to every older one — read again
		// from the original text, since the pass above rewrote v in place.
		if orig, ok := agyDecodeJSONValue(tt.Text); ok {
			if later[tt.Tool] == nil {
				later[tt.Tool] = map[string]bool{}
			}
			agyCollectRecords(orig, later[tt.Tool])
		}
	}
	return out
}

// agyDecodeJSONValue decodes one JSON document, keeping numbers as written.
func agyDecodeJSONValue(text string) (any, bool) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// agyCollectRecords adds every record (an object inside a list) at least
// agyRepeatedRecordMinBytes long, at any depth, to set.
func agyCollectRecords(v any, set map[string]bool) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if obj, ok := item.(map[string]any); ok {
				if raw, err := marshalJSONNoHTML(obj); err == nil && len(raw) >= agyRepeatedRecordMinBytes {
					set[string(raw)] = true
				}
			}
			agyCollectRecords(item, set)
		}
	case map[string]any:
		for _, item := range x {
			agyCollectRecords(item, set)
		}
	}
}

// agyPointRepeatedRecords replaces, outermost first, each record found in seen
// by a pointer, and reports whether it replaced any.
func agyPointRepeatedRecords(v any, seen map[string]bool) (any, bool) {
	changed := false
	switch x := v.(type) {
	case []any:
		for i, item := range x {
			if obj, ok := item.(map[string]any); ok {
				if raw, err := marshalJSONNoHTML(obj); err == nil && len(raw) >= agyRepeatedRecordMinBytes && seen[string(raw)] {
					x[i] = agyStubWith(obj, agyRepeatedKey)
					changed = true
					continue
				}
			}
			if nv, c := agyPointRepeatedRecords(item, seen); c {
				x[i] = nv
				changed = true
			}
		}
	case map[string]any:
		for k, item := range x {
			if nv, c := agyPointRepeatedRecords(item, seen); c {
				x[k] = nv
				changed = true
			}
		}
	}
	return v, changed
}

// agyReducibleRecord reports whether a record still has something to give up
// when reduced: a nested list or object, or a scalar too long to keep.
func agyReducibleRecord(obj map[string]any) bool {
	if _, done := obj[agyStubKey]; done {
		return false
	}
	if _, pointer := obj[agyRepeatedKey]; pointer {
		return false
	}
	for k, v := range obj {
		if k == agyDroppedFieldsKey {
			continue
		}
		switch v.(type) {
		case map[string]any, []any:
			return true
		}
		if raw, err := marshalJSONNoHTML(v); err == nil && len(raw) > agyStubScalarBytes {
			return true
		}
	}
	return false
}

// agyStubOf keeps a record's own short facts and marks it reduced.
func agyStubOf(obj map[string]any) map[string]any {
	return agyStubWith(obj, agyStubKey)
}

// agyStubWith keeps a record's own short facts under the given marker.
func agyStubWith(obj map[string]any, marker string) map[string]any {
	stub := map[string]any{marker: true}
	for k, v := range obj {
		switch v.(type) {
		case map[string]any, []any:
			continue
		}
		if raw, err := marshalJSONNoHTML(v); err == nil && len(raw) <= agyStubScalarBytes {
			stub[k] = v
		}
	}
	return stub
}

// stubTrailingRecords reduces the trailing quarter (at least one) of the whole
// records in the heaviest list that still has two or more, and reports whether
// it changed anything. The first whole record of a list is never reduced here.
// A list that carries offered times is left alone, as everywhere in the fitter
// (agy_offers.go): reducing a therapist's record would take their times with it.
func stubTrailingRecords(refs []jsonRef) bool {
	best, bestSize := -1, 0
	for i, r := range refs {
		if r.isString || len(r.arr) < 2 || r.size <= bestSize || agyHoldsClockList(r.arr) {
			continue
		}
		whole := 0
		for _, item := range r.arr {
			if obj, ok := item.(map[string]any); ok && agyReducibleRecord(obj) {
				whole++
			}
		}
		if whole >= 2 {
			best, bestSize = i, r.size
		}
	}
	if best < 0 {
		return false
	}
	arr := refs[best].arr
	var whole []int
	for i, item := range arr {
		if obj, ok := item.(map[string]any); ok && agyReducibleRecord(obj) {
			whole = append(whole, i)
		}
	}
	n := len(whole) / 4
	if n < 1 {
		n = 1
	}
	if n > len(whole)-1 {
		n = len(whole) - 1
	}
	for _, i := range whole[len(whole)-n:] {
		arr[i] = agyStubOf(arr[i].(map[string]any))
	}
	return true
}

// dropTrailingStubs removes the trailing half (at least one) of the reduced
// records from the heaviest list that still has a whole record ahead of them,
// folding the count into the list's omission note, and reports whether it
// changed anything. It is handed only the lists the fitter may thin — below
// the record level that excludes the shallowest list, the result's own
// records, whose ids the next call has to name (agyMinRecordKeys). Deeper
// down, a record that already gave up its detail goes before the last whole
// record gives up any of its own. Prod 2026-09-29 (conv 61411): with the order
// the other way round, the Botox section kept fourteen service names and lost
// the four prices of the one service the customer asked about.
func dropTrailingStubs(refs []jsonRef) bool {
	best, bestSize := -1, 0
	for i, r := range refs {
		if r.isString || len(r.arr) < 2 || r.size <= bestSize || r.setArr == nil {
			continue
		}
		stubs, whole := 0, 0
		for _, item := range r.arr {
			if obj, ok := item.(map[string]any); ok {
				if _, s := obj[agyStubKey]; s {
					stubs++
				} else {
					whole++
				}
			}
		}
		if stubs > 0 && whole > 0 {
			best, bestSize = i, r.size
		}
	}
	if best < 0 {
		return false
	}
	items := refs[best].arr
	alreadyDropped := 0
	if n := len(items); n > 0 {
		if s, ok := items[n-1].(string); ok && strings.HasPrefix(s, agyOmissionMarker) {
			if m := agyOmissionCountRe.FindStringSubmatch(s); m != nil {
				alreadyDropped, _ = strconv.Atoi(m[1])
			}
			items = items[:n-1]
		}
	}
	var stubIdx []int
	for i, item := range items {
		if obj, ok := item.(map[string]any); ok {
			if _, s := obj[agyStubKey]; s {
				stubIdx = append(stubIdx, i)
			}
		}
	}
	drop := (len(stubIdx) + 1) / 2
	gone := map[int]bool{}
	for _, i := range stubIdx[len(stubIdx)-drop:] {
		gone[i] = true
	}
	next := make([]any, 0, len(items)-drop+1)
	for i, item := range items {
		if !gone[i] {
			next = append(next, item)
		}
	}
	next = append(next, fmt.Sprintf("%s%d entries dropped by the system to fit the context window]", agyOmissionMarker, alreadyDropped+drop))
	refs[best].setArr(next)
	return true
}
