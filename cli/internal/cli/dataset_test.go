// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/dataset"
)

func TestParseAzURL(t *testing.T) {
	cases := []struct {
		in                         string
		account, container, prefix string
		wantErr                    bool
	}{
		{"az://acct/cont/pre/fix", "acct", "cont", "pre/fix", false},
		{"az://acct/cont", "acct", "cont", "", false},
		{"az://acct", "acct", "", "", false},
		{"az://acct/cont/p/", "acct", "cont", "p", false},
		{"az://", "", "", "", true},
		{"https://acct", "", "", "", true},
	}
	for _, c := range cases {
		a, cont, p, err := parseAzURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseAzURL(%q): want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAzURL(%q): %v", c.in, err)
			continue
		}
		if a != c.account || cont != c.container || p != c.prefix {
			t.Errorf("parseAzURL(%q) = %q,%q,%q; want %q,%q,%q", c.in, a, cont, p, c.account, c.container, c.prefix)
		}
	}
}

func TestParseTagPairs(t *testing.T) {
	got, err := parseTagPairs([]string{"a=1", "b=two"})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "1" || got["b"] != "two" {
		t.Fatalf("unexpected tags: %v", got)
	}
	if _, err := parseTagPairs([]string{"bad"}); err == nil {
		t.Fatal("want error on malformed tag")
	}
	if _, err := parseTagPairs([]string{"=v"}); err == nil {
		t.Fatal("want error on empty key")
	}
}

func sampleRef() dataset.ResolvedReference {
	return dataset.ResolvedReference{
		Name:    "fineweb-sample-10bt",
		Version: "v1",
		Purpose: dataset.PurposePretrain,
		Digest:  "sha256:abc",
		Modes: dataset.ReferenceModes{
			NodeLocalStage: dataset.NodeLocalStage{
				Account:   "dsacct",
				Container: "datasets",
				Prefix:    "fineweb/10bt",
				Files: []dataset.StageFile{
					{Path: "shard_0000.bin", SHA256: "aa", Bytes: 200, TokenCount: 100},
					{Path: "shard_0001.bin", SHA256: "bb", Bytes: 400, TokenCount: 200},
				},
			},
		},
		Recommended: "node_local_stage",
	}
}

func TestWriteRefEnv_StagedRoot(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRefEnv(&buf, sampleRef(), "FINEWEB_DATASET", "/staging/fw", ""); err != nil {
		t.Fatal(err)
	}
	firstURI, err := fileURIFromPath(filepath.Join("/staging/fw", "shard_0000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	secondURI, err := fileURIFromPath(filepath.Join("/staging/fw", "shard_0001.bin"))
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"FINEWEB_DATASET_URIS=",
		"FINEWEB_DATASET_SHA256S=aa,bb",
		"FINEWEB_DATASET_TOKEN_COUNTS=100,200",
		firstURI,
		secondURI,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("env output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteRefEnv_BaseURL(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRefEnv(&buf, sampleRef(), "FINEWEB_DATASET", "", "https://proxy/d"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "FINEWEB_DATASET_URIS=https://proxy/d/shard_0000.bin,https://proxy/d/shard_0001.bin") {
		t.Errorf("unexpected base-url URIs:\n%s", out)
	}
}

func TestWriteRefEnv_RequiresStagingOrBaseURL(t *testing.T) {
	var buf bytes.Buffer
	err := writeRefEnv(&buf, sampleRef(), "FINEWEB_DATASET", "", "")
	if err == nil {
		t.Fatal("want error when neither --staged-root nor --base-url is set")
	}
	if !strings.Contains(err.Error(), "SAS") {
		t.Errorf("error should explain the no-SAS policy, got: %v", err)
	}
}

func TestWriteRefEnv_RejectsZeroTokenCount(t *testing.T) {
	ref := sampleRef()
	ref.Modes.NodeLocalStage.Files[1].TokenCount = 0
	var buf bytes.Buffer
	err := writeRefEnv(&buf, ref, "FINEWEB_DATASET", "/x", "")
	if err == nil {
		t.Fatal("want error when a shard has a zero token count")
	}
	if !strings.Contains(err.Error(), "token_count") {
		t.Errorf("error should mention token_count, got: %v", err)
	}
}

func TestWriteRefEnv_CustomPrefix(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRefEnv(&buf, sampleRef(), "MYDS", "/x", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "MYDS_URIS=") {
		t.Errorf("custom prefix not applied:\n%s", buf.String())
	}
}

// fakeSource implements dataSource for verify tests.
type fakeSource struct {
	files []hashedFile
}

func (f fakeSource) scan(ctx context.Context) ([]hashedFile, error) { return f.files, nil }
func (f fakeSource) describe() string                               { return "fake" }

func verifyFixtureRecord() dataset.Record {
	rec := dataset.Record{
		SchemaVersion: dataset.SchemaVersion,
		Name:          "ds",
		Version:       "v1",
		Purpose:       dataset.PurposePretrain,
		Files: []dataset.File{
			{Path: "a.bin", Bytes: 200, SHA256: "aa"},
			{Path: "b.bin", Bytes: 400, SHA256: "bb"},
		},
	}
	rec.Digest = rec.ComputeDigest()
	return rec
}

func TestVerifyRecord_OK(t *testing.T) {
	rec := verifyFixtureRecord()
	src := fakeSource{files: []hashedFile{
		{Path: "a.bin", Bytes: 200, SHA256: "aa"},
		{Path: "b.bin", Bytes: 400, SHA256: "bb"},
	}}
	res, err := verifyRecord(context.Background(), rec, src)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("expected OK, got mismatches: %v", res.Mismatches)
	}
	if res.ActualDigest != rec.Digest {
		t.Errorf("digest mismatch: %s != %s", res.ActualDigest, rec.Digest)
	}
}

func TestVerifyRecord_HashMismatch(t *testing.T) {
	rec := verifyFixtureRecord()
	src := fakeSource{files: []hashedFile{
		{Path: "a.bin", Bytes: 200, SHA256: "aa"},
		{Path: "b.bin", Bytes: 400, SHA256: "ZZ"},
	}}
	res, err := verifyRecord(context.Background(), rec, src)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("expected verification failure on sha mismatch")
	}
}

func TestVerifyRecord_MissingFile(t *testing.T) {
	rec := verifyFixtureRecord()
	src := fakeSource{files: []hashedFile{
		{Path: "a.bin", Bytes: 200, SHA256: "aa"},
	}}
	res, err := verifyRecord(context.Background(), rec, src)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("expected failure on missing file")
	}
	found := false
	for _, m := range res.Mismatches {
		if strings.Contains(m, "missing") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a 'missing' mismatch, got %v", res.Mismatches)
	}
}

func TestVerifyRecord_ExtraFile(t *testing.T) {
	rec := verifyFixtureRecord()
	src := fakeSource{files: []hashedFile{
		{Path: "a.bin", Bytes: 200, SHA256: "aa"},
		{Path: "b.bin", Bytes: 400, SHA256: "bb"},
		{Path: "c.bin", Bytes: 600, SHA256: "cc"},
	}}
	res, err := verifyRecord(context.Background(), rec, src)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("expected failure when source has an extra file not in the record")
	}
	found := false
	for _, m := range res.Mismatches {
		if strings.Contains(m, "extra file") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an 'extra file' mismatch, got %v", res.Mismatches)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:             "512B",
		1024:            "1.0KiB",
		1024 * 1024:     "1.0MiB",
		5 * 1024 * 1024: "5.0MiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q; want %q", in, got, want)
		}
	}
}

func TestShortDigest(t *testing.T) {
	if got := shortDigest("sha256:0123456789abcdef"); got != "0123456789ab" {
		t.Errorf("shortDigest = %q", got)
	}
}
