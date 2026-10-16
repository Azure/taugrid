// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

const defaultResultFile = "e2e-results.jsonl"

var (
	globalOnce    sync.Once
	globalEmitter ResultEmitter
	globalErr     error
)

func Init() (ResultEmitter, error) {
	globalOnce.Do(func() {
		path := os.Getenv("E2E_RESULT_FILE")
		if path == "" {
			path = defaultResultFile
		}

		jsonl, err := NewJSONLSink(path)
		if err != nil {
			globalErr = err
			return
		}

		emitters := []ResultEmitter{jsonl}

		if uri := os.Getenv("E2E_KUSTO_URI"); uri != "" {
			db := os.Getenv("E2E_KUSTO_DB")
			if db == "" {
				db = "CITests"
			}
			if RunID() == 0 && inCI() {
				fmt.Fprintln(os.Stderr, "results: WARNING — Kusto sink enabled in CI but no run ID resolved (GITHUB_RUN_ID / BUILD_BUILDID both unset); TestOutcomes rows will collide under RunID=0. Wire the run-id env var.")
			}
			ks, err := NewKustoSink(uri, db)
			if err != nil {
				fmt.Fprintf(os.Stderr, "results: kusto sink init failed (continuing without): %v\n", err)
			} else {
				emitters = append(emitters, ks)
			}
		}

		globalEmitter = &Multi{Emitters: emitters}
	})
	return globalEmitter, globalErr
}

func FlushAll() {
	if globalEmitter != nil {
		if err := globalEmitter.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "results: flush error: %v\n", err)
		}
	}
}

func RunID() int64 {
	s := os.Getenv("GITHUB_RUN_ID")
	if s == "" {
		s = os.Getenv("BUILD_BUILDID")
	}
	if s == "" {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func RunAttempt() int64 {
	s := os.Getenv("GITHUB_RUN_ATTEMPT")
	if s == "" {
		s = os.Getenv("SYSTEM_JOBATTEMPT")
	}
	if s == "" {
		return 1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func Branch() string {
	if head := os.Getenv("GITHUB_HEAD_REF"); head != "" {
		return head
	}
	if ref := os.Getenv("GITHUB_REF_NAME"); ref != "" {
		return ref
	}
	if adoPR := os.Getenv("SYSTEM_PULLREQUEST_SOURCEBRANCH"); adoPR != "" {
		return strings.TrimPrefix(adoPR, "refs/heads/")
	}
	return os.Getenv("BUILD_SOURCEBRANCHNAME")
}

func inCI() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true" ||
		os.Getenv("TF_BUILD") == "True" ||
		os.Getenv("CI") == "true"
}
