// Package jsonutil validates JSON distinctions that struct decoding would discard.
package jsonutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Validate rejects duplicate members, trailing documents and (for metadata)
// null values, before decoding structs can silently discard those distinctions.
func Validate(raw []byte, rejectNull bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value func() error
	value = func() error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if t == nil && rejectNull {
			return fmt.Errorf("null is not allowed")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return fmt.Errorf("expected JSON object key")
				}
				if keys[name] {
					return fmt.Errorf("duplicate JSON key %q", name)
				}
				keys[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = dec.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}
