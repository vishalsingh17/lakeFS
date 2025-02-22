package operations

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/treeverse/lakefs/pkg/block"
	"github.com/treeverse/lakefs/pkg/catalog"
	gatewayerrors "github.com/treeverse/lakefs/pkg/gateway/errors"
	"github.com/treeverse/lakefs/pkg/gateway/serde"
	"github.com/treeverse/lakefs/pkg/graveler"
	"github.com/treeverse/lakefs/pkg/httputil"
	"github.com/treeverse/lakefs/pkg/permissions"
)

type GetObject struct{}

func (controller *GetObject) RequiredPermissions(_ *http.Request, repoID, _, path string) (permissions.Node, error) {
	return permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadObjectAction,
			Resource: permissions.ObjectArn(repoID, path),
		},
	}, nil
}

func (controller *GetObject) Handle(w http.ResponseWriter, req *http.Request, o *PathOperation) {
	o.Incr("get_object", o.Principal, o.Repository.Name, o.Reference)
	query := req.URL.Query()
	if _, exists := query["versioning"]; exists {
		o.EncodeResponse(w, req, serde.VersioningConfiguration{}, http.StatusOK)
		return
	}

	if _, exists := query["tagging"]; exists {
		o.EncodeResponse(w, req, serde.Tagging{}, http.StatusOK)
		return
	}

	beforeMeta := time.Now()
	entry, err := o.Catalog.GetEntry(req.Context(), o.Repository.Name, o.Reference, o.Path, catalog.GetEntryParams{})
	metaTook := time.Since(beforeMeta)
	o.Log(req).
		WithField("took", metaTook).
		WithError(err).
		Debug("metadata operation to retrieve object done")

	if errors.Is(err, graveler.ErrNotFound) {
		// TODO: create distinction between missing repo & missing key
		_ = o.EncodeError(w, req, gatewayerrors.Codes.ToAPIErr(gatewayerrors.ErrNoSuchKey))
		return
	}
	if errors.Is(err, catalog.ErrExpired) {
		_ = o.EncodeError(w, req, gatewayerrors.Codes.ToAPIErr(gatewayerrors.ErrNoSuchVersion))
		return
	}
	if err != nil {
		_ = o.EncodeError(w, req, gatewayerrors.Codes.ToAPIErr(gatewayerrors.ErrInternalError))
		return
	}

	o.SetHeader(w, "Last-Modified", httputil.HeaderTimestamp(entry.CreationDate))
	o.SetHeader(w, "ETag", httputil.ETag(entry.Checksum))
	o.SetHeader(w, "Accept-Ranges", "bytes")
	amzMetaWriteHeaders(w, entry.Metadata)
	// TODO: the rest of https://docs.aws.amazon.com/en_pv/AmazonS3/latest/API/API_GetObject.html
	// range query
	var expected int64
	var data io.ReadCloser
	var rng httputil.Range
	// range query
	rangeSpec := req.Header.Get("Range")
	if len(rangeSpec) > 0 {
		rng, err = httputil.ParseRange(rangeSpec, entry.Size)
		if err != nil {
			o.Log(req).WithError(err).WithField("range", rangeSpec).Debug("invalid range spec")
		}
	}
	if rangeSpec == "" || err != nil {
		// assemble a response body (range-less query)
		expected = entry.Size
		data, err = o.BlockStore.Get(req.Context(), block.ObjectPointer{StorageNamespace: o.Repository.StorageNamespace, Identifier: entry.PhysicalAddress}, entry.Size)
	} else {
		expected = rng.EndOffset - rng.StartOffset + 1 // both range ends are inclusive
		data, err = o.BlockStore.GetRange(req.Context(), block.ObjectPointer{StorageNamespace: o.Repository.StorageNamespace, Identifier: entry.PhysicalAddress}, rng.StartOffset, rng.EndOffset)
		o.SetHeader(w, "Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.StartOffset, rng.EndOffset, entry.Size))
	}
	if err != nil {
		_ = o.EncodeError(w, req, gatewayerrors.Codes.ToAPIErr(gatewayerrors.ErrInternalError))
		return
	}
	defer func() {
		_ = data.Close()
	}()
	o.SetHeader(w, "Content-Length", fmt.Sprintf("%d", expected))
	o.SetHeader(w, "Content-Type", entry.ContentType)
	_, err = io.Copy(w, data)
	if err != nil {
		o.Log(req).WithError(err).Error("could not write response body for object")
	}
}
