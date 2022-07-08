// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package minio

import (
	"encoding/xml"

	"storj.io/gateway-mt/pkg/server/gw"
	"storj.io/minio/cmd"
)

// bucketWithAttribution represents the response's building block for custom
// ListBucketsWithAttribution action.
type bucketWithAttribution struct {
	Name         string
	Attribution  string
	CreationDate string // 2006-01-02T15:04:05.000Z
}

// listBucketsWithAttributionResponse represents a response for custom
// ListBucketsWithAttribution action.
type listBucketsWithAttributionResponse struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult" json:"-"`

	Owner cmd.Owner

	Buckets struct { // A container for one or more buckets with attribution
		Buckets []bucketWithAttribution `xml:"Bucket"` // Buckets are nested
	}
}

// generateListBucketsWithAttributionResponse generates XML and
// JSON-serializable ListBucketsWithAttributionResponse from a slice of
// BucketWithAttributionInfo.
func generateListBucketsWithAttributionResponse(info []gw.BucketWithAttributionInfo) listBucketsWithAttributionResponse {
	response := listBucketsWithAttributionResponse{
		Owner: cmd.Owner{
			ID:          globalMinioDefaultOwnerID,
			DisplayName: "minio",
		},
	}

	for _, v := range info {
		response.Buckets.Buckets = append(response.Buckets.Buckets, bucketWithAttribution{
			Name:         v.Name,
			Attribution:  v.Attribution,
			CreationDate: v.Created.UTC().Format(iso8601TimeFormat),
		})
	}

	return response
}
