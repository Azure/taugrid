// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Azure/taugrid/tests/e2e/internal/ncclimage"
)

func main() {
	image := flag.String("image", "", "exact repository@linux-amd64-child-digest reference")
	repository := flag.String("repository", "", "independently approved exact repository")
	indexDigest := flag.String("index-digest", "", "qualified parent image-index digest")
	linuxAMD64Digest := flag.String("linux-amd64-digest", "", "qualified linux/amd64 child manifest digest")
	configDigest := flag.String("config-digest", "", "qualified linux/amd64 image config digest")
	flag.Parse()

	if err := ncclimage.Validate(*image, *repository, *indexDigest, *linuxAMD64Digest, *configDigest); err != nil {
		fmt.Fprintf(os.Stderr, "invalid NCCL/RDMA image qualification: %v\n", err)
		os.Exit(1)
	}
}
