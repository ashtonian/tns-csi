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

const (
	maxManifestBytes = 1 << 20
	maxFioBytes      = 32 << 20
	maxProcessBytes  = 4 << 10
	maxIndexBytes    = 8 << 20
)

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
	return checkTypedShape(data, shape, false)
}

func checkTypedShape(data []byte, shape reflect.Type, nullable bool) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if nullable && (shape.Kind() == reflect.Pointer || shape.Kind() == reflect.Slice || shape.Kind() == reflect.Map) {
			return nil
		}
		return errJSON
	}
	switch shape.Kind() {
	case reflect.Struct:
		return checkStruct(data, shape, nullable)
	case reflect.Pointer:
		return checkTypedShape(data, shape.Elem(), nullable)
	case reflect.Map:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return errJSON
		}
		for _, field := range fields {
			if err := checkTypedShape(field, shape.Elem(), nullable); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return errJSON
		}
		for _, item := range items {
			if err := checkTypedShape(item, shape.Elem(), nullable); err != nil {
				return err
			}
		}
	default:
	}
	return nil
}

func checkStruct(data []byte, shape reflect.Type, nullable bool) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return errJSON
	}
	for i := range shape.NumField() {
		field := shape.Field(i)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		value, ok := fields[name]
		if !ok {
			if nullable && options == "omitempty" {
				continue
			}
			return errJSON
		}
		if err := checkTypedShape(value, field.Type, nullable); err != nil {
			return err
		}
		delete(fields, name)
	}
	if len(fields) != 0 {
		return errJSON
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
