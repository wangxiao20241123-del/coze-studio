/*
 * Copyright 2025 coze-dev Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package coze

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/coze-dev/coze-studio/backend/application/base/ctxutil"
	"github.com/coze-dev/coze-studio/backend/domain/tokenlimit"
)

type tokenUsageResponse struct {
	Used          int64 `json:"used"`
	Limit         int64 `json:"limit"`
	WindowSeconds int64 `json:"window_seconds"`
}

// GetTokenUsage queries the caller's token usage in the current sliding window.
// @router /api/common/token_usage [GET]
func GetTokenUsage(ctx context.Context, c *app.RequestContext) {
	userID := ctxutil.GetUIDFromCtx(ctx)
	if userID == nil {
		invalidParamRequestResponse(c, "user session required")
		return
	}

	resp := &tokenUsageResponse{
		Used:          tokenlimit.Usage(*userID),
		Limit:         tokenlimit.Limit(),
		WindowSeconds: int64(tokenlimit.Window().Seconds()),
	}

	c.JSON(consts.StatusOK, resp)
}
