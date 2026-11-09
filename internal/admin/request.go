package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
)

// Every management mutation uses a small JSON object. Reject duplicate keys,
// nulls, unknown/case-aliased fields, trailing values and fractional integers;
// otherwise encoding/json's permissive defaults can silently change intent.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		write(w, 415, map[string]any{"error": "请使用 application/json"})
		return false
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err == nil {
		err = decodeObject(raw, v)
	}
	if err == nil {
		return true
	}
	status := http.StatusBadRequest
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		status = http.StatusRequestEntityTooLarge
	}
	write(w, status, map[string]any{"error": "JSON 请求格式无效、字段未知或内容过大"})
	return false
}

func decodeObject(raw []byte, v any) error {
	invalid := errors.New("invalid management JSON")
	kind := reflect.TypeOf(v).Elem()
	fields := make(map[string]bool)
	for i := 0; i < kind.NumField(); i++ {
		f := kind.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fields[name] = true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return invalid
	}
	seen := make(map[string]bool)
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return invalid
		}
		name, ok := token.(string)
		if !ok || !fields[name] || seen[name] {
			return invalid
		}
		seen[name] = true
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid
		}
	}
	if _, err = dec.Token(); err != nil {
		return invalid
	}
	if err = dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return invalid
	}
	return json.Unmarshal(raw, v)
}
