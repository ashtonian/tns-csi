// Package performance validates offline storage-test inputs and fio evidence.
package performance

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

var errJSON = errors.New("invalid JSON document")

// checkJSON rejects ambiguous keys before encoding/json can silently replace them.
func checkJSON(data []byte, limit int) error {
	if len(data) > limit {
		return errJSON
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return errJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkValue(decoder, 0); err != nil {
		return errJSON
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errJSON
	}
	return nil
}

// checkShape makes request fields mandatory and case-sensitive, including zero offsets.
func checkShape(data []byte, shape reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errJSON
	}
	switch shape.Kind() {
	case reflect.Struct:
		return checkStruct(data, shape)
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return errJSON
		}
		for _, item := range items {
			if err := checkShape(item, shape.Elem()); err != nil {
				return err
			}
		}
	default:
	}
	return nil
}

func checkStruct(data []byte, shape reflect.Type) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != shape.NumField() {
		return errJSON
	}
	for i := range shape.NumField() {
		field := shape.Field(i)
		value, ok := fields[field.Tag.Get("json")]
		if !ok {
			return errJSON
		}
		if err := checkShape(value, field.Type); err != nil {
			return err
		}
	}
	return nil
}

func checkValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errJSON
	}
	token, err := decoder.Token()
	if err != nil {
		return errJSON
	}
	switch token {
	case json.Delim('{'):
		return checkObject(decoder, depth+1)
	case json.Delim('['):
		for decoder.More() {
			if err := checkValue(decoder, depth+1); err != nil {
				return err
			}
		}
		if closeToken, closeErr := decoder.Token(); closeErr != nil || closeToken != json.Delim(']') {
			return errJSON
		}
	}
	return nil
}

func checkObject(decoder *json.Decoder, depth int) error {
	keys := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return errJSON
		}
		name, ok := key.(string)
		if !ok || keys[strings.ToLower(name)] {
			return errJSON
		}
		keys[strings.ToLower(name)] = true
		if err := checkValue(decoder, depth); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return errJSON
	}
	return nil
}
