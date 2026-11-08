package billingexpr

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/parser/lexer"
	"github.com/expr-lang/expr/vm"
	"github.com/shopspring/decimal"
)

// MoneyRequestSnapshot retains pricing decisions without request probe values.
// A nil decision means that condition could not be evaluated in the original
// context; replay fails if the final usage requires that condition.
type MoneyRequestSnapshot struct {
	ExpressionHash string           `json:"expression_hash"`
	Conditions     map[string]*bool `json:"conditions"`
}

func parseMoneyLiterals(body string) (map[file.Location]*big.Rat, map[file.Location]string, error) {
	tokens, err := lexer.Lex(file.NewSource(body))
	if err != nil {
		return nil, nil, fmt.Errorf("expr compile error: %w", err)
	}
	literals := make(map[file.Location]*big.Rat)
	literalText := make(map[file.Location]string)
	for _, token := range tokens {
		switch token.Value {
		case "_money_fixed", "_money_rule", "_money_condition", "_money_has_frozen_condition", "_money_frozen_condition", "_money_builtin", "_money_binary", "_money_unary", "_money_index":
			if token.Kind == lexer.Identifier {
				return nil, nil, fmt.Errorf("expr compile error: identifier %q is reserved for internal use", token.Value)
			}
		}
		if token.Kind != lexer.Number {
			continue
		}
		text := strings.ReplaceAll(token.Value, "_", "")
		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, "0x") || strings.HasPrefix(lower, "0b") || strings.HasPrefix(lower, "0o") {
			integer, ok := new(big.Int).SetString(text, 0)
			if !ok {
				return nil, nil, fmt.Errorf("expr compile error: invalid numeric literal")
			}
			literals[token.Location] = new(big.Rat).SetInt(integer)
			literalText[token.Location] = integer.String()
			continue
		}
		value, err := decimal.NewFromString(text)
		if err != nil {
			return nil, nil, fmt.Errorf("expr compile error: invalid numeric literal")
		}
		number, err := exactDecimal(value)
		if err != nil {
			return nil, nil, fmt.Errorf("expr compile error: %w", err)
		}
		literals[token.Location] = number
		literalText[token.Location] = value.String()
	}
	return literals, literalText, nil
}

func moneyBooleanExpressions(node ast.Node) map[file.Location]ast.Node {
	conditions := make(map[file.Location]ast.Node)
	ast.Find(node, func(node ast.Node) bool {
		switch part := node.(type) {
		case *ast.ConditionalNode:
			conditions[part.Cond.Location()] = part.Cond
		case *ast.BinaryNode:
			switch part.Operator {
			case "==", "!=", "<", ">", "<=", ">=", "in", "not in", "contains", "startsWith", "endsWith", "matches", "&&", "||", "and", "or":
				conditions[part.Location()] = part
			}
		case *ast.UnaryNode:
			if part.Operator == "!" || part.Operator == "not" {
				conditions[part.Location()] = part
			}
		case *ast.CallNode:
			if name, ok := part.Callee.(*ast.IdentifierNode); ok && name.Value == "has" {
				conditions[part.Location()] = part
			}
		}
		return false
	})
	return conditions
}

func moneyRequestProbe(node ast.Node) bool {
	call, ok := node.(*ast.CallNode)
	if !ok {
		return false
	}
	name, ok := call.Callee.(*ast.IdentifierNode)
	return ok && (name.Value == "param" || name.Value == "header")
}

// ValidateMoneyRequestSnapshot checks whether pricing can be replayed using
// boolean decisions without storing request probe values.
func ValidateMoneyRequestSnapshot(expression string) error {
	_, body := ParseExprVersion(expression)
	_, err := moneyRequestPredicates(body)
	return err
}

func moneyRequestPredicates(body string) (map[string]ast.Node, error) {
	_, literalText, err := parseMoneyLiterals(body)
	if err != nil {
		return nil, err
	}
	tree, err := parser.Parse(body)
	if err != nil {
		return nil, err
	}
	ast.Walk(&tree.Node, moneyConditionLiterals(literalText))
	conditions := moneyBooleanExpressions(tree.Node)
	predicates := make(map[string]ast.Node)
	covered := make(map[file.Location]bool)
	for _, condition := range conditions {
		if ast.Find(condition, moneyRequestProbe) == nil {
			continue
		}
		measured := ast.Find(condition, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallNode); ok {
				name, ok := call.Callee.(*ast.IdentifierNode)
				return ok && name.Value == "u"
			}
			if name, ok := node.(*ast.IdentifierNode); ok {
				switch name.Value {
				case "p", "c", "len", "cr", "cc", "cc1h", "cc12h", "img", "img_cr", "img_o", "ai", "ao", "image_count":
					return true
				}
			}
			return false
		})
		if measured != nil {
			continue
		}
		predicates[condition.String()] = condition
		ast.Find(condition, func(node ast.Node) bool {
			if moneyRequestProbe(node) {
				covered[node.Location()] = true
			}
			return false
		})
	}
	unsafe := ast.Find(tree.Node, func(node ast.Node) bool {
		return moneyRequestProbe(node) && !covered[node.Location()]
	})
	if unsafe != nil {
		return nil, fmt.Errorf("durable pricing requires request probes in independent boolean conditions")
	}
	return predicates, nil
}

func compileMoneyRequestSnapshot(body string, env map[string]any) (map[string]*vm.Program, error) {
	predicates, err := moneyRequestPredicates(body)
	if err != nil {
		return nil, err
	}
	programs := make(map[string]*vm.Program, len(predicates))
	for condition := range predicates {
		program, _, err := compileMoneyProgram(condition, env)
		if err != nil {
			return nil, err
		}
		programs[condition] = program
	}
	return programs, nil
}

func frozenMoneyBoolean(condition string, original ast.Node) ast.Node {
	return &ast.ConditionalNode{
		Ternary: true,
		Cond:    &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_has_frozen_condition"}, Arguments: []ast.Node{&ast.StringNode{Value: condition}}},
		Exp1:    &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_frozen_condition"}, Arguments: []ast.Node{&ast.StringNode{Value: condition}}},
		Exp2:    original,
	}
}
