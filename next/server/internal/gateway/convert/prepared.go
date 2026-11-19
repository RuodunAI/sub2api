package convert

import "fmt"

// RequestError rejects a request the conversion cannot express at all
// (CONVERT-MATRIX §2.2 class 3), whatever the loss policy.
type RequestError struct{ Cause error }

func (e *RequestError) Error() string { return e.Cause.Error() }
func (e *RequestError) Unwrap() error { return e.Cause }

// ClientError is a response conversion's answer that is not a success: the
// upstream's reply, successful as a request, means an error of the client
// protocol, which the gateway writes with Status and Body (CONTRACTS
// §121.8). It is not a conversion failure.
type ClientError struct {
	Status int
	Body   []byte
}

func (e *ClientError) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Body) }
