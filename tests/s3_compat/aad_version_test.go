// End-to-end tests for AAD v1 wiring through the S3 code paths.
//
// The white-box AEAD-layer behaviour (binding, mismatch rejection,
// flag-selects-shape) is covered in api/s3compat/aad_binding_test.go.
// These tests assert the live HTTP pipeline records the right
// AADVersion on every write path and that server-side CopyObject
// re-encrypts a v1 source under the destination identity (a verbatim
// copy would be undecryptable).
package s3_compat_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
)

// TestAADVersion_RecordedPerWritePath verifies the manifest records
// AADVersion="v1" for every gateway-encrypted write path
// (single-piece managed / public_distribution, erasure-coded,
// multipart) and the empty string for client_side and legacy
// objects, which are never bound.
func TestAADVersion_RecordedPerWritePath(t *testing.T) {
	t.Run("managed_single_piece", func(t *testing.T) {
		s := newEncryptionServer(t, encryptionPlacement{
			backend:        "local_fs_dev",
			encryptionMode: "managed",
		}, nil)
		putAndExpectAAD(t, s, "single.txt", []byte("managed single-piece"), "v1")
	})

	t.Run("public_distribution", func(t *testing.T) {
		s := newEncryptionServer(t, encryptionPlacement{
			backend:        "local_fs_dev",
			encryptionMode: "public_distribution",
		}, nil)
		putAndExpectAAD(t, s, "public.txt", []byte("public distribution body"), "v1")
	})

	t.Run("erasure_coded", func(t *testing.T) {
		s := newEncryptionServer(t, encryptionPlacement{
			backend:        "local_fs_dev",
			encryptionMode: "managed",
			erasureProfile: "6+2",
		}, nil)
		putAndExpectAAD(t, s, "ec.bin", bytes.Repeat([]byte("EC"), 4096), "v1")
	})

	t.Run("multipart", func(t *testing.T) {
		s := newEncryptionServer(t, encryptionPlacement{
			backend:        "local_fs_dev",
			encryptionMode: "managed",
		}, nil)
		key := "mp.bin"
		putMultipart(t, s, key, [][]byte{
			bytes.Repeat([]byte("A"), 5120),
			bytes.Repeat([]byte("B"), 5120),
		})
		m := s.firstManifest(t, s.bucket, key)
		if m.Encryption.AADVersion != "v1" {
			t.Fatalf("multipart manifest AADVersion = %q, want v1", m.Encryption.AADVersion)
		}
	})

	t.Run("client_side_unbound", func(t *testing.T) {
		s := newEncryptionServer(t, encryptionPlacement{
			backend:        "local_fs_dev",
			encryptionMode: "client_side",
		}, nil)
		key := "cs.txt"
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(key),
			Body:     bytes.NewReader(mustClientCiphertext(t, []byte("strict zk body"))),
			Metadata: map[string]string{"zk-encryption": client_sdk.ContentAlgorithm},
		}); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		m := s.firstManifest(t, s.bucket, key)
		if m.Encryption.AADVersion != "" {
			t.Fatalf("client_side manifest AADVersion = %q, want empty (gateway never sealed it)", m.Encryption.AADVersion)
		}
	})

	t.Run("legacy_unbound", func(t *testing.T) {
		s := newEncryptionServer(t, encryptionPlacement{
			backend: "local_fs_dev",
		}, nil)
		key := "legacy.txt"
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader([]byte("legacy body")),
		}); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		m := s.firstManifest(t, s.bucket, key)
		if m.Encryption.AADVersion != "" {
			t.Fatalf("legacy manifest AADVersion = %q, want empty", m.Encryption.AADVersion)
		}
	})
}

// TestAADVersion_CopyReencryptsV1Source proves CopyObject of a v1
// source re-encrypts under the destination identity rather than
// reusing the source ciphertext: the copy must be readable (a
// verbatim copy would fail the AEAD tag because the destination has a
// different version), and its manifest must carry a fresh wrapped DEK
// and a distinct version while still recording AADVersion="v1".
func TestAADVersion_CopyReencryptsV1Source(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	srcKey := "copy-src.txt"
	dstKey := "copy-dst.txt"
	plaintext := []byte("v1 object must survive a server-side copy via re-encryption")

	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(srcKey),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject src: %v", err)
	}
	src := s.firstManifest(t, s.bucket, srcKey)
	if src.Encryption.AADVersion != "v1" {
		t.Fatalf("precondition: src AADVersion = %q, want v1", src.Encryption.AADVersion)
	}

	if _, err := s.client.CopyObject(context.Background(), &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(s.bucket + "/" + srcKey),
	}); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}

	// The copy must round-trip. This is the decisive check: a
	// verbatim ciphertext copy would fail to open here because the
	// destination identity (new version) differs from the AAD the
	// source ciphertext was sealed with.
	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(dstKey),
	})
	if err != nil {
		t.Fatalf("GetObject dst: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read dst body: %v", err)
	}
	got.Body.Close()
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("copy round-trip mismatch: want %q got %q", plaintext, body)
	}

	dst := s.firstManifest(t, s.bucket, dstKey)
	if dst.Encryption.AADVersion != "v1" {
		t.Fatalf("dst AADVersion = %q, want v1", dst.Encryption.AADVersion)
	}
	if dst.VersionID == src.VersionID {
		t.Fatalf("dst VersionID = %q must differ from src (re-encrypt binds to a new identity)", dst.VersionID)
	}
	if bytes.Equal(dst.Encryption.WrappedDEK, src.Encryption.WrappedDEK) {
		t.Fatal("dst WrappedDEK equals src: copy reused the source DEK instead of re-encrypting")
	}
}

// putAndExpectAAD PUTs body under key, round-trips it, and asserts
// the manifest's recorded AADVersion.
func putAndExpectAAD(t *testing.T, s *encryptionServer, key string, body []byte, wantAAD string) {
	t.Helper()
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	// Round-trip to confirm the recorded AADVersion actually opens.
	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	rt, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got.Body.Close()
	if !bytes.Equal(rt, body) {
		t.Fatalf("round-trip mismatch for %s", key)
	}
	m := s.firstManifest(t, s.bucket, key)
	if m.Encryption.AADVersion != wantAAD {
		t.Fatalf("%s manifest AADVersion = %q, want %q", key, m.Encryption.AADVersion, wantAAD)
	}
}

// putMultipart drives a full create/upload/complete cycle.
func putMultipart(t *testing.T, s *encryptionServer, key string, parts [][]byte) {
	t.Helper()
	create, err := s.client.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)
	completed := make([]types.CompletedPart, 0, len(parts))
	for i, p := range parts {
		num := int32(i + 1)
		res, uerr := s.client.UploadPart(context.Background(), &s3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(num),
			Body:       bytes.NewReader(p),
		})
		if uerr != nil {
			t.Fatalf("UploadPart %d: %v", num, uerr)
		}
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(num), ETag: res.ETag})
	}
	if _, err := s.client.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
}
