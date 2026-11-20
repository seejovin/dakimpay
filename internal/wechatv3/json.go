package wechatv3

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Unknown additive fields are accepted. Ambiguous spellings and duplicate
// object members cannot overwrite the signed account or financial terms.
func checkObject(raw []byte) error {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || b[0] != '{' {
		return errors.New("invalid WeChat Pay JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := checkValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing WeChat Pay response data")
	}
	return nil
}
func checkValue(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("WeChat Pay JSON nesting limit exceeded")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			t, err = d.Token()
			if err != nil {
				return err
			}
			key, ok := t.(string)
			if !ok || seen[key] || strings.ToLower(key) != key || len(key) > 128 {
				return errors.New("ambiguous WeChat Pay object field")
			}
			seen[key] = true
			if err = checkValue(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err = checkValue(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}
