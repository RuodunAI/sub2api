# Golden vectors for sdk/reqsign

Two independent sources. reqsign signs with the official signer
(`aws-sdk-go-v2`, `aws/signer/v4`, the version in `next/sdk/go.mod`) and
decides which headers it gets to sign; the vectors pin both that choice and
the signer's output across upgrades.

## aws-sig-v4-test-suite/ (official)

Cases copied unchanged from AWS's published Signature Version 4 test suite
(`aws-sig-v4-test-suite`, 2015-08-30), as redistributed in
`boto/botocore` under `tests/unit/auth/aws4_testsuite/`. Each case has the
request (`.req`), the canonical request (`.creq`), the string to sign (`.sts`)
and the `Authorization` header (`.authz`). Credentials, region, service and
time are the suite's: `AKIDEXAMPLE` /
`wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY`, `us-east-1`, `service`,
`20150830T123600Z`. The tests read the canonical request and the string to
sign from the signer's own log (`SignerOptions.LogSigning`).

Only cases whose signed headers are the ones reqsign signs (host, x-amz-date,
content-type, x-amz-security-token) and whose path needs no escaping are
used. The suite's path cases (get-space, get-utf8, normalize-path/*) describe
a path that is escaped once from its raw form; for every service but S3 the
wire path is escaped a second time, which the recorded vectors below pin.

## aws-sdk-go-v2.json (recorded from the official SDK)

`Authorization` headers produced by `aws-sdk-go-v2` v1.41.0
(`aws/signer/v4.Signer.SignHTTP`) for Bedrock-shaped requests with a fixed
time and the suite's example credentials: a model id with `:`, an inference
profile id, an ARN with `:` and `/`, a session token, the OpenAI-compatible
path, a query string, and explicit ports. The SDK signs every header it is
handed plus `content-length`; the recording gave it only the headers reqsign
signs and `ContentLength = 0`, which is what reqsign hands the signer for any
request, so the whole `Authorization` value must match byte for byte.
