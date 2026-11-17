# Recorded frames

`aws-sdk-go-v2.json` holds event stream messages encoded by the official SDK
(`github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream` v1.7.4,
`Encoder.Encode`), hex-encoded, next to the string headers and payload they
were built from. They cover the shapes Bedrock's
InvokeModelWithResponseStream sends (chunk events, an exception, an error),
messages without headers or payload, and one message using every header value
type. The package under test does not import the SDK; the decoder must read
these bytes and the encoder must reproduce the string-only ones exactly.
