package controller

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type batchBillingTestResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RequestId     string `json:"request_id"`
		Quota         int    `json:"quota"`
		BillingSource string `json:"billing_source"`
		Duplicate     bool   `json:"duplicate"`
	} `json:"data"`
}

// openBatchBillingTestDB runs the production migration twice against the
// dialect's database, proving the batch billing table migrates idempotently.
func openBatchBillingTestDB(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousCache, previousRedis, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath
	previousLogConsume := common.LogConsumeEnabled
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath = previousMaster, previousCache, previousRedis, previousSQLite
		common.LogConsumeEnabled = previousLogConsume
	})
	// The full migration leaves every table behind, so MySQL and PostgreSQL
	// use dedicated databases instead of the shared empty TEST_*_DSN ones.
	dsn := ""
	if dialect != "sqlite" {
		env := "TEST_BATCH_BILLING_" + strings.ToUpper(dialect) + "_DSN"
		dsn = os.Getenv(env)
		if dsn == "" {
			t.Skipf("%s is not configured", env)
		}
	}
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = true, false, false
	common.LogConsumeEnabled = true
	common.SQLitePath = filepath.Join(t.TempDir(), "batch-billing.db")
	require.NoError(t, i18n.Init())
	for range 2 {
		require.NoError(t, model.InitDB())
		require.NoError(t, model.InitLogDB())
	}
	db := model.DB
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	var version string
	query := "SELECT version()"
	if dialect == "sqlite" {
		query = "SELECT sqlite_version()"
	}
	require.NoError(t, db.Raw(query).Scan(&version).Error)
	t.Logf("database=%s version=%s", common.MainDatabaseType(), version)
	require.True(t, db.Migrator().HasIndex(&model.BatchBillingRecord{}, "RequestId"), "request_id is unique")
	return db
}

func setBatchBillingTestPricing(t *testing.T) {
	t.Helper()
	modelRatio := ratio_setting.ModelRatio2JSONString()
	completionRatio := ratio_setting.CompletionRatio2JSONString()
	groupRatio := ratio_setting.GroupRatio2JSONString()
	specialUsable := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
	special := specialUsable.MarshalJSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(modelRatio))
		require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(completionRatio))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(groupRatio))
		require.NoError(t, types.LoadFromJsonString(specialUsable, special))
	})
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"batch-test-model":2}`))
	require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(`{"batch-test-model":4}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"svip":1,"batch":0.5}`))
	require.NoError(t, types.LoadFromJsonString(specialUsable, `{"restricted":{"-:batch":"remove"}}`))
}

func TestBatchBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openBatchBillingTestDB(t, dialect)
			setBatchBillingTestPricing(t)

			suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
			admin := model.User{Username: "batch-admin-" + suffix, Password: "unused", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "ba" + suffix}
			customer := model.User{Username: "batch-customer-" + suffix, Password: "unused", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "bc" + suffix, Quota: 10000}
			restricted := model.User{Username: "batch-restricted-" + suffix, Password: "unused", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "restricted", AffCode: "br" + suffix, Quota: 10000}
			for _, user := range []*model.User{&admin, &customer, &restricted} {
				require.NoError(t, db.Create(user).Error)
			}
			batchKey := model.Token{UserId: customer.Id, Name: "batch-key", Key: "batchkey" + suffix, Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 5000, Group: constant.BatchGroup}
			defaultKey := model.Token{UserId: customer.Id, Name: "default-key", Key: "defaultkey" + suffix, Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true, Group: "default"}
			require.NoError(t, db.Create(&batchKey).Error)
			require.NoError(t, db.Create(&defaultKey).Error)
			adminToken, _ := createScopedAccessToken(t, admin.Id, 0, "batch_billing:write")
			// The customer's default group serves these models; svip is not
			// usable by the customer, so its model is neither listed nor billable.
			channelId := int(time.Now().UnixNano() % 1_000_000_000)
			for _, ability := range []model.Ability{
				{Group: "default", Model: "batch-test-model", ChannelId: channelId, Enabled: true},
				{Group: "default", Model: "unpriced-model", ChannelId: channelId, Enabled: true},
				{Group: "default", Model: "other-model", ChannelId: channelId, Enabled: true},
				{Group: "svip", Model: "svip-only-model", ChannelId: channelId, Enabled: true},
			} {
				require.NoError(t, db.Create(&ability).Error)
			}
			t.Cleanup(func() {
				require.NoError(t, db.Where("channel_id = ?", channelId).Delete(&model.Ability{}).Error)
				userIds := []int{admin.Id, customer.Id, restricted.Id}
				require.NoError(t, db.Where("user_id IN ?", userIds).Delete(&model.BatchBillingRecord{}).Error)
				require.NoError(t, db.Where("user_id IN ?", userIds).Delete(&model.Log{}).Error)
				require.NoError(t, db.Where("user_id IN ?", userIds).Delete(&model.AuditLog{}).Error)
				require.NoError(t, db.Where("user_id IN ?", userIds).Delete(&model.UserAccessToken{}).Error)
				require.NoError(t, db.Unscoped().Where("user_id IN ?", userIds).Delete(&model.Token{}).Error)
				require.NoError(t, db.Unscoped().Where("id IN ?", userIds).Delete(&model.User{}).Error)
			})

			router := gin.New()
			router.Use(middleware.RequestId())
			router.POST("/api/batch/billing", middleware.AdminAuth(), ChargeBatchBilling)
			router.GET("/v1/models", middleware.TokenAuth(), func(c *gin.Context) { ListModels(c, constant.ChannelTypeOpenAI) })
			// The relay stand-in records what a batch relay hands to the relay
			// pipeline: the forwarded body, the routing groups, the price ratio
			// and the group its consume log is filed under.
			var relayed struct {
				body, usingGroup, routedGroup string
				autoGroups                    []string
				groupRatio                    float64
			}
			router.POST("/v1/chat/completions", middleware.TokenAuth(), func(c *gin.Context) {
				body, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				relayed.body = string(body)
				relayed.usingGroup = common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
				relayed.autoGroups = service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
				c.Set("auto_group", "default")
				info := &relaycommon.RelayInfo{UserGroup: "default", UsingGroup: relayed.usingGroup}
				relayed.groupRatio = helper.HandleGroupRatio(c, info).GroupRatio
				relayed.routedGroup = info.UsingGroup
				model.RecordConsumeLog(c, admin.Id, model.RecordConsumeLogParams{ModelName: "batch-test-model", Quota: 1, Group: info.UsingGroup, Other: model.NewLogOther()})
				c.Status(http.StatusNoContent)
			})
			charge := func(body string) (int, batchBillingTestResponse) {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/api/batch/billing", bytes.NewBufferString(body))
				request.Header.Set("Authorization", "Bearer "+adminToken)
				router.ServeHTTP(response, request)
				var result batchBillingTestResponse
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result), response.Body.String())
				return response.Code, result
			}
			balances := func() (int, int) {
				var user model.User
				require.NoError(t, db.First(&user, customer.Id).Error)
				var token model.Token
				require.NoError(t, db.First(&token, batchKey.Id).Error)
				return user.Quota, token.RemainQuota
			}

			t.Run("batch keys list their billable models and relay only with the batch marker", func(t *testing.T) {
				listModels := func(key string) (int, []string) {
					response := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
					request.Header.Set("Authorization", "Bearer sk-"+key)
					router.ServeHTTP(response, request)
					var result struct {
						Data []struct {
							Id string `json:"id"`
						} `json:"data"`
					}
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result), response.Body.String())
					ids := make([]string, 0, len(result.Data))
					for _, item := range result.Data {
						ids = append(ids, item.Id)
					}
					return response.Code, ids
				}
				// Unpriced models are hidden from the list as for any other key.
				status, ids := listModels(batchKey.Key)
				assert.Equal(t, http.StatusOK, status)
				assert.Equal(t, []string{"batch-test-model"}, ids)
				status, ids = listModels(defaultKey.Key)
				assert.Equal(t, http.StatusOK, status)
				assert.Equal(t, []string{"batch-test-model"}, ids)

				relay := func(key, contentType, body string) int {
					response := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
					request.Header.Set("Authorization", "Bearer sk-"+key)
					request.Header.Set("Content-Type", contentType)
					router.ServeHTTP(response, request)
					return response.Code
				}
				for _, body := range []string{`{}`, `{"model":"batch-test-model","batch":false}`, `{"model":"batch-test-model","batch":"true"}`, `{"model":"batch-test-model","extra":{"batch":true}}`} {
					assert.Equal(t, http.StatusForbidden, relay(batchKey.Key, "application/json", body), body)
				}
				assert.Equal(t, http.StatusForbidden, relay(batchKey.Key, "multipart/form-data; boundary=x", "--x\r\nContent-Disposition: form-data; name=\"batch\"\r\n\r\ntrue\r\n--x--\r\n"))

				require.Equal(t, http.StatusNoContent, relay(batchKey.Key, "application/json", `{"model":"batch-test-model","batch":true,"messages":[]}`))
				assert.JSONEq(t, `{"model":"batch-test-model","messages":[]}`, relayed.body, "the marker never reaches the provider")
				assert.Equal(t, "auto", relayed.usingGroup)
				assert.Equal(t, []string{"default"}, relayed.autoGroups)
				assert.Equal(t, "default", relayed.routedGroup)
				assert.EqualValues(t, 0.5, relayed.groupRatio, "priced at the batch ratio")
				var relayLog model.Log
				require.NoError(t, db.Where("user_id = ? AND type = ?", admin.Id, model.LogTypeConsume).Last(&relayLog).Error)
				assert.Equal(t, constant.BatchGroup, relayLog.Group)
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(relayLog.Other, &other))
				assert.Equal(t, true, other["batch_billing"])
				assert.Equal(t, "default", other["batch_route_group"])

				// An ordinary key relays as before and is priced by its own group.
				require.Equal(t, http.StatusNoContent, relay(defaultKey.Key, "application/json", `{"model":"batch-test-model","batch":true}`))
				assert.Equal(t, "default", relayed.usingGroup)
				assert.EqualValues(t, 1, relayed.groupRatio)
			})

			t.Run("batch group is offered only when editing keys", func(t *testing.T) {
				for _, tc := range []struct {
					userId  int
					query   string
					offered bool
				}{
					{customer.Id, "", false},
					{customer.Id, "?scene=token", true},
					{restricted.Id, "?scene=token", false},
				} {
					response := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(response)
					c.Request = httptest.NewRequest(http.MethodGet, "/api/user/self/groups"+tc.query, nil)
					c.Set("id", tc.userId)
					GetUserGroups(c)
					var result struct {
						Data map[string]map[string]any `json:"data"`
					}
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
					group, offered := result.Data[constant.BatchGroup]
					assert.Equal(t, tc.offered, offered, "user %d %q", tc.userId, tc.query)
					if offered {
						assert.EqualValues(t, 0.5, group["ratio"])
					}
				}
			})

			t.Run("batch keys can be created unless the group is removed", func(t *testing.T) {
				for userId, success := range map[int]bool{customer.Id: true, restricted.Id: false} {
					response := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(response)
					c.Request = httptest.NewRequest(http.MethodPost, "/api/token/", bytes.NewBufferString(`{"name":"created-batch-key","group":"batch","unlimited_quota":true,"expired_time":-1}`))
					c.Set("id", userId)
					AddToken(c)
					var result batchBillingTestResponse
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
					assert.Equal(t, success, result.Success, result.Message)
					var count int64
					require.NoError(t, db.Model(&model.Token{}).Where("user_id = ? AND name = ?", userId, "created-batch-key").Count(&count).Error)
					assert.Equal(t, success, count == 1)
				}
			})

			t.Run("usage is priced with the batch group ratio", func(t *testing.T) {
				// (prompt 1000 + completion 200 * completion ratio 4) * model ratio 2 * batch ratio 0.5
				status, result := charge(`{"key":"sk-` + batchKey.Key + `","request_id":"job-1","model_name":"batch-test-model","prompt_tokens":1000,"completion_tokens":200,"note":"line 1"}`)
				require.Equal(t, http.StatusOK, status, result.Message)
				assert.True(t, result.Success)
				assert.Equal(t, 1800, result.Data.Quota)
				assert.Equal(t, "wallet", result.Data.BillingSource)
				assert.False(t, result.Data.Duplicate)
				userQuota, tokenQuota := balances()
				assert.Equal(t, 8200, userQuota)
				assert.Equal(t, 3200, tokenQuota)

				var logs []model.Log
				require.NoError(t, db.Where("user_id = ? AND type = ?", customer.Id, model.LogTypeConsume).Find(&logs).Error)
				require.Len(t, logs, 1)
				assert.Equal(t, constant.BatchGroup, logs[0].Group)
				assert.Equal(t, "batch-key", logs[0].TokenName)
				assert.Equal(t, customer.Username, logs[0].Username)
				assert.Equal(t, "batch-test-model", logs[0].ModelName)
				assert.Equal(t, 1800, logs[0].Quota)
				assert.Equal(t, 1000, logs[0].PromptTokens)
				assert.Equal(t, 200, logs[0].CompletionTokens)
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
				assert.Equal(t, true, other["batch_billing"])
				assert.Equal(t, "job-1", other["batch_request_id"])
				assert.EqualValues(t, 0.5, other["group_ratio"])
			})

			t.Run("a repeated request_id is charged once", func(t *testing.T) {
				status, result := charge(`{"key":"` + batchKey.Key + `","request_id":"job-1","model_name":"batch-test-model","prompt_tokens":1000,"completion_tokens":200}`)
				require.Equal(t, http.StatusOK, status, result.Message)
				assert.True(t, result.Data.Duplicate)
				assert.Equal(t, 1800, result.Data.Quota)
				userQuota, tokenQuota := balances()
				assert.Equal(t, 8200, userQuota)
				assert.Equal(t, 3200, tokenQuota)
				var count int64
				require.NoError(t, db.Model(&model.Log{}).Where("user_id = ? AND type = ?", customer.Id, model.LogTypeConsume).Count(&count).Error)
				assert.EqualValues(t, 1, count)
			})

			t.Run("explicit charges", func(t *testing.T) {
				status, result := charge(`{"key":"` + batchKey.Key + `","request_id":"job-2","model_name":"batch-test-model","quota":200}`)
				require.Equal(t, http.StatusOK, status, result.Message)
				assert.Equal(t, 200, result.Data.Quota)
				status, result = charge(`{"key":"` + batchKey.Key + `","request_id":"job-3","model_name":"batch-test-model","amount":"0.002"}`)
				require.Equal(t, http.StatusOK, status, result.Message)
				assert.Equal(t, common.QuotaFromFloat(0.002*common.QuotaPerUnit), result.Data.Quota)

				// A zero charge is accepted and logged without moving funds.
				userBefore, tokenBefore := balances()
				for i, body := range []string{`"quota":0`, `"amount":"0"`} {
					status, result = charge(`{"key":"` + batchKey.Key + `","request_id":"job-zero-` + strconv.Itoa(i) + `","model_name":"batch-test-model",` + body + `}`)
					require.Equal(t, http.StatusOK, status, result.Message)
					assert.True(t, result.Success)
					assert.Zero(t, result.Data.Quota)
				}
				userAfter, tokenAfter := balances()
				assert.Equal(t, userBefore, userAfter)
				assert.Equal(t, tokenBefore, tokenAfter)
				var zeroLogs int64
				require.NoError(t, db.Model(&model.Log{}).Where("user_id = ? AND type = ? AND quota = 0", customer.Id, model.LogTypeConsume).Count(&zeroLogs).Error)
				assert.EqualValues(t, 2, zeroLogs)
			})

			t.Run("rejections move no funds", func(t *testing.T) {
				userBefore, tokenBefore := balances()
				for _, tc := range []struct {
					body   string
					status int
					code   string
				}{
					{`{"key":"` + batchKey.Key + `","request_id":"job-4","model_name":"batch-test-model","quota":4000}`, http.StatusPaymentRequired, "insufficient_key_quota"},
					{`{"key":"` + defaultKey.Key + `","request_id":"job-5","model_name":"batch-test-model","quota":1}`, http.StatusForbidden, "key_not_batch"},
					{`{"key":"missing","request_id":"job-6","model_name":"batch-test-model","quota":1}`, http.StatusNotFound, "invalid_key"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-7","model_name":"unpriced-model","prompt_tokens":10}`, http.StatusBadRequest, "model_price_error"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-13","model_name":"svip-only-model","quota":1}`, http.StatusForbidden, "model_unavailable"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-14","model_name":"no-channel-model","quota":1}`, http.StatusForbidden, "model_unavailable"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-8","model_name":"batch-test-model","quota":1,"prompt_tokens":10}`, http.StatusBadRequest, "invalid_request"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-9","model_name":"batch-test-model","prompt_tokens":-1,"completion_tokens":5}`, http.StatusBadRequest, "invalid_request"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-10","model_name":"batch-test-model","quota":2147483648}`, http.StatusBadRequest, "quota_out_of_range"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-11","model_name":"batch-test-model","amount":"-1"}`, http.StatusBadRequest, "quota_out_of_range"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-15","model_name":"batch-test-model","quota":-1}`, http.StatusBadRequest, "quota_out_of_range"},
					{`{"key":"` + batchKey.Key + `","request_id":"job-1","model_name":"other-model","quota":1}`, http.StatusConflict, "request_id_conflict"},
				} {
					status, result := charge(tc.body)
					assert.Equal(t, tc.status, status, tc.body)
					assert.Equal(t, tc.code, result.Code, tc.body)
					assert.False(t, result.Success)
				}
				userAfter, tokenAfter := balances()
				assert.Equal(t, userBefore, userAfter)
				assert.Equal(t, tokenBefore, tokenAfter)
				var released int64
				require.NoError(t, db.Model(&model.BatchBillingRecord{}).Where("request_id = ?", "job-4").Count(&released).Error)
				assert.Zero(t, released, "a rejected charge releases its request_id")

				require.NoError(t, db.Model(&model.Token{}).Where("id = ?", batchKey.Id).Update("unlimited_quota", true).Error)
				status, result := charge(`{"key":"` + batchKey.Key + `","request_id":"job-12","model_name":"batch-test-model","quota":9000}`)
				assert.Equal(t, http.StatusPaymentRequired, status)
				assert.Equal(t, "insufficient_quota", result.Code)
				userAfter, _ = balances()
				assert.Equal(t, userBefore, userAfter)
			})
		})
	}
}
