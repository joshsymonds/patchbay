// Package jsonstrict holds the one structural rule patchbay applies to a JSON
// document before anything decodes it: exactly one complete value, and no
// object naming the same member twice.
//
// It knows nothing about schemas. Which names a document may carry, which of
// them are required, and what their values must be belong to the package that
// owns the document; this one answers only whether the bytes say the same thing
// twice.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// rootPath names the whole value, so a repeat at the root reads the way a
// nested one does.
const rootPath = "$"

// RejectDuplicateNames reports whether data is exactly one JSON value whose
// every object names each of its members once.
//
// JSON member names are case-sensitive and exact, and a name said twice is
// ambiguous: encoding/json keeps whichever came last, so a document that says a
// thing twice decodes as its second saying with nothing left of the first. The
// check therefore runs over the raw bytes, before a decode erases the evidence,
// and recurses through every object value and every array element — an object
// inside an array is as ambiguous as one at the root. Repeated scalars are not
// a concept: only names can collide.
//
// encoding/json's own syntax and type errors are wrapped rather than replaced,
// so a caller can still tell malformed bytes from ambiguous ones.
func RejectDuplicateNames(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Nothing here reads a value, and parsing numbers is how a document holding
	// one too large for a float64 would be refused by a check about names.
	decoder.UseNumber()

	if err := checkValue(decoder, rootPath); err != nil {
		return err
	}
	// Reading once past the value tells the three ends apart. Clean EOF is the
	// document ending where it should. An error is encoding/json's account of
	// bytes it could not parse, wrapped like every other one so the reason
	// survives; replacing it with the refusal below would tell a caller the bytes
	// were surplus when they were malformed. Anything that parses IS surplus: one
	// more value nobody will look at.
	if _, err := decoder.Token(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode JSON after the value: %w", err)
	}
	return errors.New("unexpected trailing data after the JSON value")
}

// checkValue reads one value, descending into it when it has members to check.
// A scalar is read and dropped: it is the names above it that matter.
func checkValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON at %s: %w", path, err)
	}
	delim, structured := token.(json.Delim)
	if !structured {
		return nil
	}
	switch delim {
	case '{':
		return checkObject(decoder, path)
	case '[':
		return checkArray(decoder, path)
	default:
		return fmt.Errorf("decode JSON at %s: unexpected %q", path, delim)
	}
}

func checkObject(decoder *json.Decoder, path string) error {
	named := map[string]struct{}{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode JSON at %s: %w", path, err)
		}
		name, ok := token.(string)
		if !ok {
			return fmt.Errorf("decode JSON at %s: member name %v is not a string", path, token)
		}
		if _, repeated := named[name]; repeated {
			return fmt.Errorf("%s has a duplicate member %q", path, name)
		}
		named[name] = struct{}{}
		if valueErr := checkValue(decoder, path+"."+name); valueErr != nil {
			return valueErr
		}
	}
	return closingToken(decoder, path)
}

func checkArray(decoder *json.Decoder, path string) error {
	for index := 0; decoder.More(); index++ {
		if err := checkValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
			return err
		}
	}
	return closingToken(decoder, path)
}

// closingToken consumes the } or ] the loop above stopped at, which is also
// where a document that ends early surfaces as one.
func closingToken(decoder *json.Decoder, path string) error {
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("decode JSON at %s: %w", path, err)
	}
	return nil
}
