//go:build !unix

package conformance

import "errors"

func untouchedBuffer(int) ([]byte, func(), error) {
	return nil, nil, errors.New("anonymous mappings are unavailable")
}
