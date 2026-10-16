package service

import (
	"strings"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// BatchRelayMarker is the top-level JSON member a batch group key sends to
// call a relay route. It is removed before the request reaches a provider.
const BatchRelayMarker = "batch"

// ConsumeBatchRelayMarker reports whether the JSON request body opts into a
// batch relay with a top-level "batch": true, and strips that member from the
// body. Form and multipart bodies never opt in.
func ConsumeBatchRelayMarker(c *gin.Context) (bool, error) {
	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if strings.Contains(contentType, "multipart/") || strings.Contains(contentType, "x-www-form-urlencoded") {
		return false, nil
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return false, err
	}
	body, err := storage.Bytes()
	if err != nil {
		return false, err
	}
	if !gjson.ValidBytes(body) {
		return false, nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() || root.Get(BatchRelayMarker).Type != gjson.True {
		return false, nil
	}
	stripped, err := sjson.DeleteBytes(body, BatchRelayMarker)
	if err != nil {
		return false, err
	}
	return true, common.ReplaceRequestBody(c, stripped)
}
