package signer

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// Headers is a text scalar for configuration and secret files. It accepts
// comma-separated Header: value entries, with CSV quoting for values containing
// commas. Header names are case-insensitive and repeated names retain all values.
// This follows Boa's typed-secret example and go-livepeer's header syntax.
type Headers http.Header

func validHeader(name, value string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	for _, c := range value {
		if (c < 32 && c != '\t') || c == 127 {
			return false
		}
	}
	return true
}

func (h *Headers) UnmarshalText(text []byte) error {
	parsed := make(http.Header)
	input := strings.TrimSpace(string(text))
	if input != "" {
		reader := csv.NewReader(strings.NewReader(input))
		reader.TrimLeadingSpace = true
		reader.FieldsPerRecord = -1
		entries, err := reader.Read()
		if err != nil {
			return errors.New("invalid comma-separated webhook headers")
		}
		if _, err := reader.Read(); err != io.EOF {
			return errors.New("webhook headers must be one comma-separated record")
		}
		for _, entry := range entries {
			name, value, ok := strings.Cut(entry, ":")
			name, value = strings.TrimSpace(name), strings.TrimSpace(value)
			if !ok || !validHeader(name, value) {
				return errors.New("expected webhook headers in Header: value form")
			}
			parsed.Add(name, value)
		}
	}
	*h = Headers(parsed)
	return nil
}

// MarshalText sorts names for deterministic secret-file comparisons and auth
// policy fingerprints, and quotes entries containing commas or quotation marks.
func (h Headers) MarshalText() ([]byte, error) {
	var entries []string
	for _, name := range slices.Sorted(maps.Keys(h)) {
		if !validHeader(name, "") {
			return nil, errors.New("invalid webhook header name")
		}
		for _, value := range h[name] {
			if !validHeader(name, value) {
				return nil, errors.New("invalid webhook header name or value")
			}
			entries = append(entries, name+": "+value)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write(entries); err != nil {
		return nil, err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
