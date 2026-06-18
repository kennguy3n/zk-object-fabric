// Package client_sdk provides the client-side encryption SDK that
// implements Strict ZK mode (docs/PROPOSAL.md §3.7): per-object DEK
// generation, BLAKE3 chunking, XChaCha20-Poly1305 sealing, and
// CMK-wrapped DEK exchange over the S3-compatible API.
package client_sdk
