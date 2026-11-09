package billingexpr

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/conf"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/parser/lexer"
	"github.com/expr-lang/expr/vm"
	"github.com/shopspring/decimal"
)

// MoneyInput carries normalized whole token counts and frozen request context.
// A zero Time uses pkg/clock.Now when the evaluation starts.
type MoneyInput struct {
	Tokens map[string]int64
	// ImageCount overrides the request estimate with a bounded actual payload
	// count, including zero. It never changes frozen param("n") probes.
	ImageCount *int
	// MeanImageCount supplies an exact historical mean for margin estimates.
	// It is never an actual payload count or a request multiplier override.
	MeanImageCount *big.Rat
	// TokenQuantities overrides whole counts for exact weighted cache quantities,
	// such as the 1.6 short-write units used by an unpriced one-hour cache write.
	TokenQuantities map[string]decimal.Decimal
	// RationalTokenQuantities preserves exact means for pricing estimates.
	RationalTokenQuantities map[string]*big.Rat
	Request                 RequestInput
	Time                    time.Time
}

// MoneyRequestRuleTrace preserves a request multiplier as an exact source decimal.
type MoneyRequestRuleTrace struct {
	Cond       string `json:"cond"`
	Multiplier string `json:"multiplier"`
	Matched    bool   `json:"matched"`
}

// MoneyConditionTrace freezes a pricing decision without retaining probe values.
type MoneyConditionTrace struct {
	Cond    string `json:"cond"`
	Matched bool   `json:"matched"`
}

// MoneyTraceResult contains billing metadata without request bodies or headers.
// FixedPrice is present even for an explicitly free request-priced leaf.
type MoneyTraceResult struct {
	UnmeasuredTokens    []string                `json:"unmeasured_tokens,omitempty"`
	Currency            money.Currency          `json:"currency,omitempty"`
	UnroundedAmount     string                  `json:"unrounded_amount,omitempty"`
	Conditions          []MoneyConditionTrace   `json:"conditions,omitempty"`
	ImageCount          *int                    `json:"image_count,omitempty"`
	MeanImageCount      string                  `json:"mean_image_count,omitempty"`
	FixedPricePerImage  bool                    `json:"fixed_price_per_image,omitempty"`
	ImageQuantityPriced bool                    `json:"image_quantity_priced,omitempty"`
	BillingUnit         BillingUnit             `json:"billing_unit"`
	FixedPrice          *string                 `json:"fixed_price,omitempty"`
	MatchedTier         string                  `json:"matched_tier"`
	RequestRules        []MoneyRequestRuleTrace `json:"request_rules,omitempty"`
}

// MoneyUsedVars returns source identifiers for exact token normalization. It
// includes unselected branches before the legacy float optimizer can fold a
// decimal comparison, and returns a copy so callers cannot alter cached data.
func MoneyUsedVars(expression string) (map[string]bool, error) {
	entry, err := compileEntryFromCacheByHash(expression, ExprHashString(expression))
	if err != nil {
		return nil, err
	}
	return maps.Clone(entry.sourceVars), nil
}

// RunMoneyExpr evaluates v1 prices in the requested currency. Token prices are
// per million tokens; fixed prices are per request. It never evaluates a price
// with RunExpr or converts through legacy quota units.
func RunMoneyExpr(expression string, input MoneyInput, currency money.Currency) (money.Value, MoneyTraceResult, error) {
	trace := MoneyTraceResult{BillingUnit: BillingUnitToken, Currency: currency}
	// Reuse the public language's grammar and diagnostics, but never execute its
	// floating-point program to calculate currency.
	entry, err := compileEntryFromCacheByHash(expression, ExprHashString(expression))
	if err != nil {
		return money.Value{}, trace, err
	}
	headers := normalizeHeaders(input.Request.Headers)
	now := input.Time
	if now.IsZero() {
		now = clock.Now()
	}
	tokenValues := make(map[string]*big.Rat)
	env := map[string]any{
		"_money_token": func(name string) (*big.Rat, error) {
			if input.Request.UnmeasuredTokens[name] {
				trace.UnmeasuredTokens = append(trace.UnmeasuredTokens, name)
				return nil, fmt.Errorf("token quantity %s is unknown", name)
			}
			return tokenValues[name], nil
		},
		"tier": func(name string, amount any) any {
			trace.MatchedTier = name
			return amount
		},
		"_money_fixed": func(value any, text string, perImage bool) (*big.Rat, error) {
			amount, err := exactNumber(value)
			if err != nil {
				return nil, err
			}
			trace.BillingUnit = BillingUnitRequest
			trace.FixedPrice = &text
			trace.FixedPricePerImage = perImage
			return new(big.Rat).Mul(amount, big.NewRat(1_000_000, 1)), nil
		},
		"_money_image_quantity": func(value any) any {
			trace.ImageQuantityPriced = true
			return value
		},
		"param": func(path string) (any, error) {
			path = strings.TrimSpace(path)
			if path == "" || len(input.Request.Body) == 0 {
				return nil, nil
			}
			return exactJSON(ojson.GetBytes(input.Request.Body, path))
		},
		"header": func(key string) (string, error) {
			key = strings.ToLower(strings.TrimSpace(key))
			for _, deferred := range input.Request.DeferredHeaders {
				if strings.EqualFold(key, strings.TrimSpace(deferred)) {
					return "", fmt.Errorf("pricing header is unavailable before account preparation")
				}
			}
			return headers[key], nil
		},
		"has": func(source any, substr string) bool {
			return source != nil && substr != "" && strings.Contains(exactString(source), substr)
		},
		"_money_condition": func(condition string, value any) (bool, error) {
			matched, ok := value.(bool)
			if !ok {
				return false, fmt.Errorf("pricing condition must be boolean")
			}
			trace.Conditions = append(trace.Conditions, MoneyConditionTrace{Cond: condition, Matched: matched})
			return matched, nil
		},
		"_money_rule": func(index int, matched bool, multiplier, fallback any) any {
			if matched {
				trace.RequestRules[index].Matched = true
				return multiplier
			}
			return fallback
		},
		"max":            func(a, b any) (*big.Rat, error) { return exactMath("max", a, b) },
		"min":            func(a, b any) (*big.Rat, error) { return exactMath("min", a, b) },
		"abs":            func(a any) (*big.Rat, error) { return exactMath("abs", a) },
		"ceil":           func(a any) (*big.Rat, error) { return exactMath("ceil", a) },
		"floor":          func(a any) (*big.Rat, error) { return exactMath("floor", a) },
		"hour":           func(tz string) int { return moneyTimeInZone(now, tz).Hour() },
		"minute":         func(tz string) int { return moneyTimeInZone(now, tz).Minute() },
		"weekday":        func(tz string) int { return int(moneyTimeInZone(now, tz).Weekday()) },
		"month":          func(tz string) int { return int(moneyTimeInZone(now, tz).Month()) },
		"day":            func(tz string) int { return moneyTimeInZone(now, tz).Day() },
		"_money_builtin": exactBuiltin,
		"_money_binary":  exactBinary,
		"_money_unary":   exactUnary,
		"_money_index":   exactIndex,
	}
	imageCount := 1
	if entry.sourceVars["image_count"] {
		if input.Request.ImageCount != nil {
			imageCount = *input.Request.ImageCount
		}
		if input.ImageCount != nil {
			imageCount = *input.ImageCount
			if imageCount < 0 || imageCount > dto.MaxImageN {
				return money.Value{}, trace, fmt.Errorf("actual image_count must be between 0 and %d", dto.MaxImageN)
			}
		} else if imageCount < 1 || imageCount > dto.MaxImageN {
			return money.Value{}, trace, fmt.Errorf("image_count must be between 1 and %d", dto.MaxImageN)
		}
		trace.ImageCount = &imageCount
	}
	env["image_count"] = big.NewRat(int64(imageCount), 1)
	if entry.sourceVars["image_count"] && input.MeanImageCount != nil {
		if input.ImageCount != nil || input.MeanImageCount.Sign() < 0 || input.MeanImageCount.Cmp(big.NewRat(dto.MaxImageN, 1)) > 0 {
			return money.Value{}, trace, fmt.Errorf("mean image_count must be between 0 and %d without an actual count", dto.MaxImageN)
		}
		env["image_count"] = new(big.Rat).Set(input.MeanImageCount)
		trace.ImageCount = nil
		trace.MeanImageCount = input.MeanImageCount.RatString()
	}
	for name, count := range input.Tokens {
		if count < 0 {
			return money.Value{}, trace, fmt.Errorf("%s must be non-negative", name)
		}
	}
	quantities := make(map[string]*big.Rat, len(input.TokenQuantities))
	for name, quantity := range input.TokenQuantities {
		number, err := exactDecimal(quantity)
		if err != nil {
			return money.Value{}, trace, err
		}
		if number.Sign() < 0 || number.Cmp(big.NewRat(math.MaxInt64, 1)) > 0 {
			return money.Value{}, trace, fmt.Errorf("%s must be between 0 and %d", name, int64(math.MaxInt64))
		}
		quantities[name] = number
	}
	for name, quantity := range input.RationalTokenQuantities {
		if quantity == nil || quantity.Sign() < 0 || quantity.Cmp(big.NewRat(math.MaxInt64, 1)) > 0 {
			return money.Value{}, trace, fmt.Errorf("%s has an invalid rational quantity", name)
		}
		quantities[name] = new(big.Rat).Set(quantity)
	}
	for _, name := range []string{"p", "c", "len", "cr", "cc", "cc1h", "cc12h", "img", "img_cr", "img_o", "ai", "ao"} {
		tokenValues[name] = big.NewRat(input.Tokens[name], 1)
		if quantity, ok := quantities[name]; ok {
			tokenValues[name] = quantity
		}
		env[name] = tokenValues[name]
	}
	entry.moneyOnce.Do(func() {
		_, body := ParseExprVersion(expression)
		entry.moneyProgram, entry.moneyRequestRules, entry.moneyErr = compileMoneyProgram(body, env)
	})
	if entry.moneyErr != nil {
		return money.Value{}, trace, entry.moneyErr
	}
	trace.RequestRules = append([]MoneyRequestRuleTrace(nil), entry.moneyRequestRules...)
	result, err := expr.Run(entry.moneyProgram, env)
	if err != nil {
		return money.Value{}, trace, fmt.Errorf("expr run error: %w", err)
	}
	number, err := exactNumber(result)
	if err != nil {
		return money.Value{}, trace, fmt.Errorf("expr result is %T, want number", result)
	}
	if number.Sign() < 0 {
		return money.Value{}, trace, fmt.Errorf("expr result must be non-negative")
	}
	unrounded := new(big.Rat).Quo(number, big.NewRat(1_000_000, 1))
	trace.UnroundedAmount = unrounded.RatString()
	amount, err := money.NewRationalValue(unrounded, currency)
	return amount, trace, err
}

// compileMoneyProgram replaces numeric operations before optimization can lose
// source digits. Only immutable source constants and metadata enter the cache;
// request functions are always read from the current run's environment.
func compileMoneyProgram(body string, env map[string]any) (*vm.Program, []MoneyRequestRuleTrace, error) {
	tokens, err := lexer.Lex(file.NewSource(body))
	if err != nil {
		return nil, nil, fmt.Errorf("expr compile error: %w", err)
	}
	literals := make(map[file.Location]*big.Rat)
	literalText := make(map[file.Location]string)
	for _, token := range tokens {
		switch token.Value {
		case "_money_token", "_money_fixed", "_money_rule", "_money_condition", "_money_builtin", "_money_binary", "_money_unary", "_money_index", "_money_image_quantity":
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
	tree, err := parser.Parse(body)
	if err != nil {
		return nil, nil, fmt.Errorf("expr compile error: %w", err)
	}
	rules := make(map[file.Location]int)
	var requestRules []MoneyRequestRuleTrace
	ast.Find(tree.Node, func(node ast.Node) bool {
		conditional, ok := node.(*ast.ConditionalNode)
		if !ok || !conditional.Ternary || !usesRequestProbe(conditional.Cond) {
			return false
		}
		multiplier := literals[conditional.Exp1.Location()]
		fallback := literals[conditional.Exp2.Location()]
		if multiplier != nil && fallback != nil && fallback.Cmp(big.NewRat(1, 1)) == 0 {
			rules[conditional.Location()] = len(requestRules)
			ast.Walk(&conditional.Cond, moneyConditionLiterals(literalText))
			requestRules = append(requestRules, MoneyRequestRuleTrace{Cond: conditional.Cond.String(), Multiplier: literalText[conditional.Exp1.Location()]})
		}
		return false
	})
	conditions := make(map[file.Location]string)
	conditionals := make(map[file.Location]string)
	ast.Find(tree.Node, func(node ast.Node) bool {
		switch part := node.(type) {
		case *ast.ConditionalNode:
			ast.Walk(&part.Cond, moneyConditionLiterals(literalText))
			conditionals[part.Location()] = part.Cond.String()
		case *ast.BinaryNode:
			switch part.Operator {
			case "==", "!=", "<", ">", "<=", ">=", "in":
				conditions[part.Location()] = part.String()
			}
		}
		return false
	})
	program, err := expr.Compile(body, expr.Env(env), expr.Patch(&moneyPatcher{imagePricing: imagePricing(tree.Node), literals: literals, literalText: literalText, rules: rules, conditions: conditions, conditionals: conditionals}), expr.Optimize(false), expr.AsAny())
	if err != nil {
		return nil, nil, fmt.Errorf("expr compile error: %w", err)
	}
	return program, requestRules, nil
}

// The trace printer uses exact number text; this metadata tree is never run.
type moneyConditionLiterals map[file.Location]string

func (literals moneyConditionLiterals) Visit(node *ast.Node) {
	switch (*node).(type) {
	case *ast.IntegerNode, *ast.FloatNode:
		ast.Patch(node, &ast.ConstantNode{Value: json.Number(literals[(*node).Location()])})
	}
}

type moneyPatcher struct {
	literals     map[file.Location]*big.Rat
	literalText  map[file.Location]string
	rules        map[file.Location]int
	conditions   map[file.Location]string
	conditionals map[file.Location]string
	imagePricing imagePricePlan
}

func (p *moneyPatcher) Visit(node *ast.Node) {
	switch part := (*node).(type) {
	case *ast.IdentifierNode:
		switch part.Value {
		case "p", "c", "len", "cr", "cc", "cc1h", "cc12h", "img", "img_cr", "img_o", "ai", "ao":
			ast.Patch(node, &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_token"}, Arguments: []ast.Node{&ast.StringNode{Value: part.Value}}})
		}
	case *ast.IntegerNode:
		ast.Patch(node, &ast.ConstantNode{Value: int(p.literals[part.Location()].Num().Int64())})
	case *ast.FloatNode:
		ast.Patch(node, &ast.ConstantNode{Value: p.literals[part.Location()]})
	case *ast.CallNode:
		if callee, ok := part.Callee.(*ast.IdentifierNode); ok && callee.Value == "fixed" {
			ast.Patch(node, &ast.CallNode{
				Callee:    &ast.IdentifierNode{Value: "_money_fixed"},
				Arguments: []ast.Node{part.Arguments[0], &ast.StringNode{Value: p.literalText[part.Arguments[0].Location()]}, &ast.BoolNode{Value: p.imagePricing.fixedImagePrices[part.Location()]}},
			})
		}
	case *ast.ConditionalNode:
		if condition, ok := p.conditionals[part.Location()]; ok {
			part.Cond = &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_condition"}, Arguments: []ast.Node{&ast.StringNode{Value: condition}, part.Cond}}
		}
		if index, ok := p.rules[part.Location()]; ok {
			ast.Patch(node, &ast.CallNode{
				Callee:    &ast.IdentifierNode{Value: "_money_rule"},
				Arguments: []ast.Node{&ast.IntegerNode{Value: index}, part.Cond, part.Exp1, part.Exp2},
			})
		}
	case *ast.BuiltinNode:
		switch part.Name {
		case "int", "float", "string", "fromJSON", "round", "sum", "mean", "median":
			arguments := part.Arguments
			if part.Name == "sum" && len(arguments) == 2 {
				arguments = []ast.Node{&ast.BuiltinNode{Name: "map", Arguments: arguments}}
			}
			ast.Patch(node, &ast.CallNode{
				Callee:    &ast.IdentifierNode{Value: "_money_builtin"},
				Arguments: append([]ast.Node{&ast.StringNode{Value: part.Name}}, arguments...),
			})
		}
	case *ast.UnaryNode:
		if part.Operator == "-" || part.Operator == "+" {
			ast.Patch(node, &ast.CallNode{
				Callee:    &ast.IdentifierNode{Value: "_money_unary"},
				Arguments: []ast.Node{&ast.StringNode{Value: part.Operator}, part.Node},
			})
		}
	case *ast.MemberNode:
		if _, literal := part.Property.(*ast.StringNode); !literal {
			part.Property = &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_index"}, Arguments: []ast.Node{part.Property}}
		}
	case *ast.BinaryNode:
		if imageOnLeft, ok := p.imagePricing.multipliers[part.Location()]; ok {
			quantity := &part.Right
			if imageOnLeft {
				quantity = &part.Left
			}
			*quantity = &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_image_quantity"}, Arguments: []ast.Node{*quantity}}
		}
		switch part.Operator {
		case "..":
			part.Left = &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_index"}, Arguments: []ast.Node{part.Left}}
			part.Right = &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_index"}, Arguments: []ast.Node{part.Right}}
		case "+", "-", "*", "/", "%", "^", "**", "==", "!=", "<", ">", "<=", ">=", "in":
			var replacement ast.Node = &ast.CallNode{
				Callee:    &ast.IdentifierNode{Value: "_money_binary"},
				Arguments: []ast.Node{&ast.StringNode{Value: part.Operator}, part.Left, part.Right},
			}
			if condition, ok := p.conditions[part.Location()]; ok {
				replacement = &ast.CallNode{Callee: &ast.IdentifierNode{Value: "_money_condition"}, Arguments: []ast.Node{&ast.StringNode{Value: condition}, replacement}}
			}
			ast.Patch(node, replacement)
		}
	}
}

func exactBinary(operator string, left, right any) (any, error) {
	a, aErr := exactNumber(left)
	b, bErr := exactNumber(right)
	if operator == "==" || operator == "!=" {
		equal := reflect.DeepEqual(left, right)
		if aErr == nil && bErr == nil {
			equal = a.Cmp(b) == 0
		}
		return equal == (operator == "=="), nil
	}
	if operator == "in" {
		if right == nil {
			return false, nil
		}
		collection := reflect.ValueOf(right)
		switch collection.Kind() {
		case reflect.Array, reflect.Slice:
			for i := range collection.Len() {
				equal, err := exactBinary("==", left, collection.Index(i).Interface())
				if err != nil {
					return nil, err
				}
				if equal == true {
					return true, nil
				}
			}
			return false, nil
		case reflect.Map:
			key := reflect.ValueOf(left)
			if key.IsValid() && key.Type().AssignableTo(collection.Type().Key()) {
				return collection.MapIndex(key).IsValid(), nil
			}
			return false, nil
		}
		return nil, fmt.Errorf("collection required for in")
	}
	if aErr != nil || bErr != nil {
		if a, ok := left.(string); ok {
			if b, ok := right.(string); ok {
				switch operator {
				case "+":
					return a + b, nil
				case "<":
					return a < b, nil
				case "<=":
					return a <= b, nil
				case ">":
					return a > b, nil
				case ">=":
					return a >= b, nil
				}
			}
		}
		return nil, fmt.Errorf("numeric operands required")
	}
	switch operator {
	case "+":
		return new(big.Rat).Add(a, b), nil
	case "-":
		return new(big.Rat).Sub(a, b), nil
	case "*":
		return new(big.Rat).Mul(a, b), nil
	case "/":
		if b.Sign() == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return new(big.Rat).Quo(a, b), nil
	case "%":
		if b.Sign() == 0 || !a.IsInt() || !b.IsInt() {
			return nil, fmt.Errorf("modulo requires integers and a non-zero divisor")
		}
		return new(big.Rat).SetInt(new(big.Int).Rem(a.Num(), b.Num())), nil
	case "^", "**":
		return exactPower(a, b)
	case "<":
		return a.Cmp(b) < 0, nil
	case "<=":
		return a.Cmp(b) <= 0, nil
	case ">":
		return a.Cmp(b) > 0, nil
	case ">=":
		return a.Cmp(b) >= 0, nil
	}
	return nil, fmt.Errorf("unsupported numeric operator")
}

func exactUnary(operator string, value any) (*big.Rat, error) {
	number, err := exactNumber(value)
	if err != nil {
		return nil, err
	}
	if operator == "-" {
		return new(big.Rat).Neg(number), nil
	}
	return number, nil
}

func exactMath(name string, values ...any) (*big.Rat, error) {
	a, err := exactNumber(values[0])
	if err != nil {
		return nil, err
	}
	switch name {
	case "abs":
		return new(big.Rat).Abs(a), nil
	case "ceil", "floor", "round":
		integer, remainder := new(big.Int), new(big.Int)
		integer.QuoRem(a.Num(), a.Denom(), remainder)
		if name == "ceil" && remainder.Sign() > 0 {
			integer.Add(integer, big.NewInt(1))
		} else if name == "floor" && remainder.Sign() < 0 {
			integer.Sub(integer, big.NewInt(1))
		} else if name == "round" && new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(a.Denom()) >= 0 {
			integer.Add(integer, big.NewInt(int64(remainder.Sign())))
		}
		return new(big.Rat).SetInt(integer), nil
	case "max", "min":
		b, err := exactNumber(values[1])
		if err != nil {
			return nil, err
		}
		if (name == "max" && a.Cmp(b) < 0) || (name == "min" && a.Cmp(b) > 0) {
			return b, nil
		}
		return a, nil
	}
	return nil, fmt.Errorf("unknown math helper")
}

func exactPower(base, exponent *big.Rat) (*big.Rat, error) {
	if exponent.Sign() == 0 {
		return big.NewRat(1, 1), nil
	}
	if base.Sign() == 0 {
		if exponent.Sign() < 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return new(big.Rat), nil
	}
	if !exponent.IsInt() {
		if base.Sign() < 0 {
			return nil, fmt.Errorf("power does not have an exact rational result")
		}
		numerator, ok := exactIntegerRoot(base.Num(), exponent.Denom())
		if !ok {
			return nil, fmt.Errorf("power does not have an exact rational result")
		}
		denominator, ok := exactIntegerRoot(base.Denom(), exponent.Denom())
		if !ok {
			return nil, fmt.Errorf("power does not have an exact rational result")
		}
		base = new(big.Rat).SetFrac(numerator, denominator)
	}
	power := new(big.Int).Abs(exponent.Num())
	if new(big.Int).Abs(base.Num()).Cmp(base.Denom()) == 0 {
		if base.Sign() < 0 && power.Bit(0) == 1 {
			return big.NewRat(-1, 1), nil
		}
		return big.NewRat(1, 1), nil
	}
	// The VM bounds collection allocations. Exact powers need the same finite
	// budget before allocating integers whose size grows with the exponent.
	bits := uint64(max(base.Num().BitLen(), base.Denom().BitLen()))
	if !power.IsUint64() || power.Uint64() > uint64(conf.DefaultMemoryBudget)/bits {
		return nil, fmt.Errorf("exact power exceeds expression memory budget")
	}
	numerator := new(big.Int).Exp(base.Num(), power, nil)
	denominator := new(big.Int).Exp(base.Denom(), power, nil)
	if exponent.Sign() < 0 {
		numerator, denominator = denominator, numerator
	}
	return new(big.Rat).SetFrac(numerator, denominator), nil
}

func exactIntegerRoot(value, degree *big.Int) (*big.Int, bool) {
	if value.Cmp(big.NewInt(1)) <= 0 {
		return new(big.Int).Set(value), true
	}
	if !degree.IsInt64() || degree.Int64() > int64(value.BitLen()) {
		return nil, false
	}
	low := big.NewInt(1)
	high := new(big.Int).Lsh(big.NewInt(1), uint(value.BitLen()/int(degree.Int64())+1))
	for low.Cmp(high) <= 0 {
		mid := new(big.Int).Rsh(new(big.Int).Add(low, high), 1)
		comparison := new(big.Int).Exp(mid, degree, nil).Cmp(value)
		if comparison == 0 {
			return mid, true
		}
		if comparison < 0 {
			low.Add(mid, big.NewInt(1))
		} else {
			high.Sub(mid, big.NewInt(1))
		}
	}
	return nil, false
}

func exactBuiltin(name string, values ...any) (any, error) {
	switch name {
	case "string":
		return exactString(values[0]), nil
	case "fromJSON":
		text, ok := values[0].(string)
		if !ok || !ojson.Valid(text) {
			return nil, fmt.Errorf("fromJSON requires valid JSON text")
		}
		return exactJSON(ojson.Parse(text))
	case "round":
		return exactMath("round", values[0])
	case "int", "float":
		var number *big.Rat
		if text, ok := values[0].(string); ok {
			if name == "int" {
				integer, ok := new(big.Int).SetString(text, 10)
				if !ok || !integer.IsInt64() {
					return nil, fmt.Errorf("int requires a valid int64 value")
				}
				return int(integer.Int64()), nil
			}
			parsed, err := decimal.NewFromString(text)
			if err != nil {
				return nil, fmt.Errorf("float requires a finite decimal value")
			}
			number, err = exactDecimal(parsed)
			if err != nil {
				return nil, err
			}
		} else {
			var err error
			number, err = exactNumber(values[0])
			if err != nil {
				return nil, err
			}
		}
		if name == "int" {
			integer := new(big.Int).Quo(number.Num(), number.Denom())
			if !integer.IsInt64() {
				return nil, money.ErrOverflow
			}
			return int(integer.Int64()), nil
		}
		return number, nil
	case "sum", "mean", "median":
		var numbers []*big.Rat
		pending := append([]any(nil), values...)
		for len(pending) > 0 {
			value := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			collection := reflect.ValueOf(value)
			if collection.IsValid() && (collection.Kind() == reflect.Array || collection.Kind() == reflect.Slice) {
				for i := range collection.Len() {
					pending = append(pending, collection.Index(i).Interface())
				}
				continue
			}
			number, err := exactNumber(value)
			if err != nil {
				return nil, err
			}
			numbers = append(numbers, number)
		}
		if len(numbers) == 0 {
			return new(big.Rat), nil
		}
		if name == "median" {
			slices.SortFunc(numbers, func(a, b *big.Rat) int { return a.Cmp(b) })
			mid := len(numbers) / 2
			if len(numbers)%2 == 1 {
				return numbers[mid], nil
			}
			return new(big.Rat).Quo(new(big.Rat).Add(numbers[mid-1], numbers[mid]), big.NewRat(2, 1)), nil
		}
		sum := new(big.Rat)
		for _, number := range numbers {
			sum.Add(sum, number)
		}
		if name == "mean" {
			sum.Quo(sum, big.NewRat(int64(len(numbers)), 1))
		}
		return sum, nil
	}
	return nil, fmt.Errorf("unsupported numeric builtin")
}

func exactString(value any) string {
	if number, err := exactNumber(value); err == nil {
		if places, finite := number.FloatPrec(); finite {
			return number.FloatString(places)
		}
		return number.RatString()
	}
	return fmt.Sprint(value)
}

func exactNumber(value any) (*big.Rat, error) {
	switch number := value.(type) {
	case *big.Rat:
		if number != nil {
			return number, nil
		}
	case big.Rat:
		return &number, nil
	}
	reflected := reflect.ValueOf(value)
	if reflected.IsValid() {
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return new(big.Rat).SetInt64(reflected.Int()), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return new(big.Rat).SetInt(new(big.Int).SetUint64(reflected.Uint())), nil
		}
	}
	return nil, fmt.Errorf("exact numeric value required, got %T", value)
}

func exactIndex(value any) (any, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	number, err := exactNumber(value)
	if err != nil || !number.IsInt() || !number.Num().IsInt64() {
		return nil, fmt.Errorf("integer index required")
	}
	return int(number.Num().Int64()), nil
}

func moneyTimeInZone(now time.Time, zone string) time.Time {
	location, err := time.LoadLocation(strings.TrimSpace(zone))
	if err != nil || strings.TrimSpace(zone) == "" {
		return now.UTC()
	}
	return now.In(location)
}

func exactDecimal(number decimal.Decimal) (*big.Rat, error) {
	exponent := int64(number.Exponent())
	if exponent > int64(conf.DefaultMemoryBudget) || exponent < -int64(conf.DefaultMemoryBudget) {
		return nil, fmt.Errorf("exact decimal exceeds expression memory budget")
	}
	return number.Rat(), nil
}

func exactJSON(result ojson.Result) (any, error) {
	switch result.Type {
	case ojson.Number:
		number, err := decimal.NewFromString(result.Raw)
		if err != nil {
			return nil, fmt.Errorf("param returned an invalid numeric value")
		}
		return exactDecimal(number)
	case ojson.String:
		return result.Str, nil
	case ojson.True:
		return true, nil
	case ojson.False:
		return false, nil
	case ojson.JSON:
		if result.IsArray() {
			var values []any
			for item := range result.Values() {
				value, err := exactJSON(item)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			return values, nil
		}
		values := make(map[string]any)
		for key, item := range result.All() {
			value, err := exactJSON(item)
			if err != nil {
				return nil, err
			}
			values[key.Str] = value
		}
		return values, nil
	}
	return nil, nil
}
