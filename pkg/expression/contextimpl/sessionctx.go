// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package contextimpl

import (
	"time"

	"github.com/pingcap/tidb/pkg/errctx"
	exprctx "github.com/pingcap/tidb/pkg/expression/context"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/stmtctx"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/types"
)

// sessionctx.Context + *ExprCtxExtendedImpl should implement `expression.BuildContext`
// Only used to assert `ExprCtxExtendedImpl` should implement all methods not in `sessionctx.Context`
var _ exprctx.BuildContext = struct {
	sessionctx.Context
	*ExprCtxExtendedImpl
}{}

// ExprCtxExtendedImpl extends the sessionctx.Context to implement `expression.BuildContext`
type ExprCtxExtendedImpl struct {
	sctx sessionctx.Context
}

// NewExprExtendedImpl creates a new ExprCtxExtendedImpl.
func NewExprExtendedImpl(sctx sessionctx.Context) *ExprCtxExtendedImpl {
	return &ExprCtxExtendedImpl{sctx: sctx}
}

// CtxID returns the context id.
func (ctx *ExprCtxExtendedImpl) CtxID() uint64 {
	return ctx.sctx.GetSessionVars().StmtCtx.CtxID()
}

// SQLMode returns the sql mode
func (ctx *ExprCtxExtendedImpl) SQLMode() mysql.SQLMode {
	return ctx.sctx.GetSessionVars().SQLMode
}

// TypeCtx returns the types.Context
func (ctx *ExprCtxExtendedImpl) TypeCtx() types.Context {
	return ctx.sctx.GetSessionVars().StmtCtx.TypeCtx()
}

// ErrCtx returns the errctx.Context
func (ctx *ExprCtxExtendedImpl) ErrCtx() errctx.Context {
	return ctx.sctx.GetSessionVars().StmtCtx.ErrCtx()
}

// Location returns the timezone info
func (ctx *ExprCtxExtendedImpl) Location() *time.Location {
	tc := ctx.TypeCtx()
	return tc.Location()
}

// AppendWarning append warnings to the context.
func (ctx *ExprCtxExtendedImpl) AppendWarning(err error) {
	ctx.sctx.GetSessionVars().StmtCtx.AppendWarning(err)
}

// WarningCount gets warning count.
func (ctx *ExprCtxExtendedImpl) WarningCount() int {
	return int(ctx.sctx.GetSessionVars().StmtCtx.WarningCount())
}

// TruncateWarnings truncates warnings begin from start and returns the truncated warnings.
func (ctx *ExprCtxExtendedImpl) TruncateWarnings(start int) []stmtctx.SQLWarn {
	return ctx.sctx.GetSessionVars().StmtCtx.TruncateWarnings(start)
}

// CurrentDB return the current database name
func (ctx *ExprCtxExtendedImpl) CurrentDB() string {
	return ctx.sctx.GetSessionVars().CurrentDB
}

// GetMaxAllowedPacket returns the value of the 'max_allowed_packet' system variable.
func (ctx *ExprCtxExtendedImpl) GetMaxAllowedPacket() uint64 {
	return ctx.sctx.GetSessionVars().MaxAllowedPacket
}

// GetDefaultWeekFormatMode returns the value of the 'default_week_format' system variable.
func (ctx *ExprCtxExtendedImpl) GetDefaultWeekFormatMode() string {
	mode, ok := ctx.sctx.GetSessionVars().GetSystemVar(variable.DefaultWeekFormat)
	if !ok || mode == "" {
		return "0"
	}
	return mode
}
