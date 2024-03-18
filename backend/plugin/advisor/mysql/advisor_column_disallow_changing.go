package mysql

// Framework code is generated by the generator.

import (
	"fmt"

	"github.com/antlr4-go/antlr/v4"
	"github.com/pkg/errors"

	mysql "github.com/bytebase/mysql-parser"

	"github.com/bytebase/bytebase/backend/plugin/advisor"
	mysqlparser "github.com/bytebase/bytebase/backend/plugin/parser/mysql"
	storepb "github.com/bytebase/bytebase/proto/generated-go/store"
)

var (
	_ advisor.Advisor = (*ColumnDisallowChangingAdvisor)(nil)
)

func init() {
	advisor.Register(storepb.Engine_MYSQL, advisor.MySQLColumnDisallowChanging, &ColumnDisallowChangingAdvisor{})
	advisor.Register(storepb.Engine_MARIADB, advisor.MySQLColumnDisallowChanging, &ColumnDisallowChangingAdvisor{})
	advisor.Register(storepb.Engine_OCEANBASE, advisor.MySQLColumnDisallowChanging, &ColumnDisallowChangingAdvisor{})
}

// ColumnDisallowChangingAdvisor is the advisor checking for disallow CHANGE COLUMN statement.
type ColumnDisallowChangingAdvisor struct {
}

// Check checks for disallow CHANGE COLUMN statement.
func (*ColumnDisallowChangingAdvisor) Check(ctx advisor.Context, _ string) ([]advisor.Advice, error) {
	stmtList, ok := ctx.AST.([]*mysqlparser.ParseResult)
	if !ok {
		return nil, errors.Errorf("failed to convert to mysql parser result")
	}

	level, err := advisor.NewStatusBySQLReviewRuleLevel(ctx.Rule.Level)
	if err != nil {
		return nil, err
	}
	checker := &columnDisallowChangingChecker{
		level: level,
		title: string(ctx.Rule.Type),
	}

	for _, stmtNode := range stmtList {
		checker.baseLine = stmtNode.BaseLine
		antlr.ParseTreeWalkerDefault.Walk(checker, stmtNode.Tree)
	}

	if len(checker.adviceList) == 0 {
		checker.adviceList = append(checker.adviceList, advisor.Advice{
			Status:  advisor.Success,
			Code:    advisor.Ok,
			Title:   "OK",
			Content: "",
		})
	}
	return checker.adviceList, nil
}

type columnDisallowChangingChecker struct {
	*mysql.BaseMySQLParserListener

	baseLine   int
	adviceList []advisor.Advice
	level      advisor.Status
	title      string
	text       string
}

func (checker *columnDisallowChangingChecker) EnterQuery(ctx *mysql.QueryContext) {
	checker.text = ctx.GetParser().GetTokenStream().GetTextFromRuleContext(ctx)
}

func (checker *columnDisallowChangingChecker) EnterAlterTable(ctx *mysql.AlterTableContext) {
	if !mysqlparser.IsTopMySQLRule(&ctx.BaseParserRuleContext) {
		return
	}
	if ctx.AlterTableActions() == nil {
		return
	}
	if ctx.AlterTableActions().AlterCommandList() == nil {
		return
	}
	if ctx.AlterTableActions().AlterCommandList().AlterList() == nil {
		return
	}

	for _, item := range ctx.AlterTableActions().AlterCommandList().AlterList().AllAlterListItem() {
		if item == nil {
			continue
		}

		// change column
		if item.CHANGE_SYMBOL() != nil && item.ColumnInternalRef() != nil && item.Identifier() != nil {
			checker.adviceList = append(checker.adviceList, advisor.Advice{
				Status:  checker.level,
				Code:    advisor.UseChangeColumnStatement,
				Title:   checker.title,
				Content: fmt.Sprintf("\"%s\" contains CHANGE COLUMN statement", checker.text),
				Line:    checker.baseLine + item.GetStart().GetLine(),
			})
		}
	}
}
