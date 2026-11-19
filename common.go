package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
)

type J = map[string]any
type Reply struct {
	Status  int
	Body    any
	Headers map[string]string
}

func obj(v any) J {
	if x, ok := v.(map[string]any); ok && x != nil {
		return x
	}
	return J{}
}
func arr(v any) []any {
	if x, ok := v.([]any); ok && x != nil {
		return x
	}
	if v != nil {
		r := reflect.ValueOf(v)
		if r.Kind() == reflect.Slice || r.Kind() == reflect.Array {
			a := make([]any, r.Len())
			for i := range a {
				a[i] = r.Index(i).Interface()
			}
			return a
		}
	}
	return []any{}
}
func str(v any) string { s, _ := v.(string); return s }
func sdef(v any, d string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return d
}
func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) {
			return int(n)
		}
	case json.Number:
		i, _ := strconv.ParseFloat(string(n), 64)
		return int(i)
	case string:
		i, _ := strconv.ParseFloat(n, 64)
		return int(i)
	case bool:
		if n {
			return 1
		}
	}
	return 0
}
func truth(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case int64:
		return x != 0
	case json.Number:
		return x != "0"
	}
	return true
}
func js(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if e.Encode(v) != nil {
		return "null"
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte{'\n'}))
}
func out(status int, body any) Reply        { return Reply{Status: status, Body: body} }
func fail(status int, message string) Reply { return out(status, J{"message": message}) }
func missing() Reply                        { return fail(404, "Not Found") }
func jsString(v any) string {
	if v == nil {
		return "null"
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case map[string]any:
		return "[object Object]"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}
