package main

import (
	"strconv"
	"unicode/utf8"
)

// Match encoding/json with EscapeHTML(false), including invalid UTF-8 and
// JavaScript line separators. strconv.Quote alone uses Go-only escapes.
func appendJSONString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(value); {
		c := value[i]
		i++
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, `\b`...)
		case '\f':
			dst = append(dst, `\f`...)
		case '\n':
			dst = append(dst, `\n`...)
		case '\r':
			dst = append(dst, `\r`...)
		case '\t':
			dst = append(dst, `\t`...)
		default:
			if c < 32 {
				const hex = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&15])
			} else if c < utf8.RuneSelf {
				dst = append(dst, c)
			} else {
				r, n := utf8.DecodeRuneInString(value[i-1:])
				switch {
				case r == utf8.RuneError && n == 1:
					dst = append(dst, `\ufffd`...)
				case r == 0x2028:
					dst = append(dst, `\u2028`...)
				case r == 0x2029:
					dst = append(dst, `\u2029`...)
				default:
					dst = append(dst, value[i-1:i-1+n]...)
				}
				i += n - 1
			}
		}
	}
	return append(dst, '"')
}
func wireString(b []byte, key string, v any) []byte {
	b = append(b, key...)
	return appendJSONString(b, str(v))
}
func wireInt(b []byte, key string, v any) []byte {
	b = append(b, key...)
	return strconv.AppendInt(b, int64(num(v)), 10)
}
func issueJSON(b []byte, j J) []byte {
	b = wireString(b, `{"body":`, j["body"])
	b = append(b, `,"closed_at":`...)
	if j["closed_at"] == nil {
		b = append(b, "null"...)
	} else {
		b = appendJSONString(b, str(j["closed_at"]))
	}
	b = wireInt(b, `,"comments":`, j["comments"])
	b = wireString(b, `,"created_at":`, j["created_at"])
	b = wireInt(b, `,"id":`, j["id"])
	b = append(b, `,"labels":[`...)
	for i, v := range arr(j["labels"]) {
		if i > 0 {
			b = append(b, ',')
		}
		l := obj(v)
		b = wireString(b, `{"color":`, l["color"])
		b = append(b, `,"default":`...)
		b = strconv.AppendBool(b, l["default"] == true)
		b = wireString(b, `,"description":`, l["description"])
		b = wireInt(b, `,"id":`, l["id"])
		b = wireString(b, `,"name":`, l["name"])
		b = wireString(b, `,"node_id":`, l["node_id"])
		b = append(b, '}')
	}
	b = append(b, ']')
	b = wireString(b, `,"node_id":`, j["node_id"])
	b = wireInt(b, `,"number":`, j["number"])
	b = wireString(b, `,"repository_url":`, j["repository_url"])
	b = wireString(b, `,"state":`, j["state"])
	b = wireString(b, `,"title":`, j["title"])
	b = wireString(b, `,"updated_at":`, j["updated_at"])
	b = wireString(b, `,"url":`, j["url"])
	u := obj(j["user"])
	b = wireInt(b, `,"user":{"id":`, u["id"])
	b = wireString(b, `,"login":`, u["login"])
	b = wireString(b, `,"name":`, u["name"])
	b = wireString(b, `,"node_id":`, u["node_id"])
	b = append(b, `,"site_admin":`...)
	b = strconv.AppendBool(b, u["site_admin"] == true)
	b = wireString(b, `,"type":`, u["type"])
	return append(b, '}', '}')
}
