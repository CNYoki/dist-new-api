package model

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogIpRecordingFollowsUserSettingUnlessForced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := common.ForceRecordIpLogEnabled
	t.Cleanup(func() { common.ForceRecordIpLogEnabled = previous })

	for _, tc := range []struct {
		forced     bool
		userOptsIn bool
		wantIp     string
	}{
		{forced: false, userOptsIn: false, wantIp: ""},
		{forced: false, userOptsIn: true, wantIp: "203.0.113.7"},
		{forced: true, userOptsIn: false, wantIp: "203.0.113.7"},
		{forced: true, userOptsIn: true, wantIp: "203.0.113.7"},
	} {
		t.Run(fmt.Sprintf("forced=%t/user=%t", tc.forced, tc.userOptsIn), func(t *testing.T) {
			common.ForceRecordIpLogEnabled = tc.forced
			setting := `{"record_ip_log":false}`
			if tc.userOptsIn {
				setting = `{"record_ip_log":true}`
			}
			user := User{Username: "ip-" + t.Name(), Password: "unused", Status: common.UserStatusEnabled, AffCode: "ip-" + t.Name(), Setting: setting}
			require.NoError(t, DB.Create(&user).Error)
			t.Cleanup(func() {
				require.NoError(t, LOG_DB.Where("user_id = ?", user.Id).Delete(&Log{}).Error)
				require.NoError(t, DB.Unscoped().Delete(&user).Error)
			})

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request.RemoteAddr = "203.0.113.7:4321"
			RecordConsumeLog(c, user.Id, RecordConsumeLogParams{ModelName: "ip-test", Quota: 1, Other: NewLogOther()})
			RecordErrorLog(c, user.Id, 0, "ip-test", "", "failed", 0, 0, false, "default", NewLogOther())

			var logs []Log
			require.NoError(t, LOG_DB.Where("user_id = ?", user.Id).Order("type").Find(&logs).Error)
			require.Len(t, logs, 2)
			for _, log := range logs {
				assert.Equal(t, tc.wantIp, log.Ip, "log type %d", log.Type)
			}
		})
	}
}
