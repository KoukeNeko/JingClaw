package tool

import (
	"bytes"
	"errors"
	"io"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func bytesReader(raw []byte) io.Reader { return bytes.NewReader(raw) }

func asToolError(err error, target **Error) bool { return errors.As(err, target) }

func asValidationError(err error, target **jsonschema.ValidationError) bool {
	return errors.As(err, target)
}
