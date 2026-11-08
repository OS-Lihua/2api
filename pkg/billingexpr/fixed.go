package billingexpr

import (
	"fmt"
	"math"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/file"
)

// UsesFixedPricing includes unselected branches, even when compilation later
// optimizes them away. Hosts use it to reject unsupported billing entrances.
func UsesFixedPricing(expression string) bool {
	return UsesFixedPricingByHash(expression, ExprHashString(expression))
}

// UsesFixedPricingByHash avoids hashing an expression already in a snapshot.
func UsesFixedPricingByHash(expression, hash string) bool {
	entry, err := compileEntryFromCacheByHash(expression, hash)
	return err == nil && entry.fixedPricing
}

// UsesOnlyFixedPricing reports whether every pricing leaf uses fixed(),
// including branches whose conditions fail or are optimized away.
func UsesOnlyFixedPricing(expression string) bool {
	entry, err := compileEntryFromCacheByHash(expression, ExprHashString(expression))
	return err == nil && entry.fixedOnly
}

// UsesOnlyFixedRequestPricing excludes image quantity multipliers from fixed
// pricing, while allowing image_count to select a request-priced branch.
func UsesOnlyFixedRequestPricing(expression string) bool {
	entry, err := compileEntryFromCacheByHash(expression, ExprHashString(expression))
	return err == nil && entry.fixedRequestOnly
}

type imagePricePlan struct {
	multipliers      map[file.Location]bool
	fixedImagePrices map[file.Location]bool
}

// imagePricing tracks quantity factors through price branches, never probes.
// A fixed leaf knows its unit before later factors have finished evaluating.
func imagePricing(node ast.Node) imagePricePlan {
	plan := imagePricePlan{multipliers: make(map[file.Location]bool), fixedImagePrices: make(map[file.Location]bool)}
	type priceBranch struct {
		node     ast.Node
		perImage bool
	}
	pending := []priceBranch{{node: node}}
	for len(pending) > 0 {
		branch := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch part := branch.node.(type) {
		case *ast.ConditionalNode:
			pending = append(pending, priceBranch{part.Exp1, branch.perImage}, priceBranch{part.Exp2, branch.perImage})
		case *ast.BinaryNode:
			if part.Operator != "*" {
				continue
			}
			leftImage, rightImage := branch.perImage, branch.perImage
			if left, ok := part.Left.(*ast.IdentifierNode); ok && left.Value == "image_count" && containsPricingMarker(part.Right) {
				plan.multipliers[part.Location()] = true
				rightImage = true
			}
			if right, ok := part.Right.(*ast.IdentifierNode); ok && right.Value == "image_count" && containsPricingMarker(part.Left) {
				plan.multipliers[part.Location()] = false
				leftImage = true
			}
			pending = append(pending, priceBranch{part.Left, leftImage}, priceBranch{part.Right, rightImage})
		case *ast.CallNode:
			callee, ok := part.Callee.(*ast.IdentifierNode)
			if !ok {
				continue
			}
			if callee.Value == "tier" && len(part.Arguments) == 2 {
				pending = append(pending, priceBranch{part.Arguments[1], branch.perImage})
			}
			if callee.Value == "fixed" {
				plan.fixedImagePrices[part.Location()] = branch.perImage
			}
		}
	}
	return plan
}

func containsPricingMarker(node ast.Node) bool {
	return ast.Find(node, func(part ast.Node) bool {
		identifier, ok := part.(*ast.IdentifierNode)
		return ok && (identifier.Value == "tier" || identifier.Value == "fixed")
	}) != nil
}

func isRequestPriceMultiplier(node ast.Node) bool {
	if identifier, ok := node.(*ast.IdentifierNode); ok && identifier.Value == "image_count" {
		return true
	}
	conditional, ok := node.(*ast.ConditionalNode)
	if !ok || !usesRequestProbe(conditional.Cond) || containsPricingMarker(conditional.Cond) {
		return false
	}
	multiplier, multiplierOK := requestRuleNumber(conditional.Exp1)
	fallback, fallbackOK := requestRuleNumber(conditional.Exp2)
	return multiplierOK && fallbackOK && fallback == 1 && multiplier >= 0 && !math.IsNaN(multiplier) && !math.IsInf(multiplier, 0)
}

// validateFixedPricingTree enforces one pricing leaf per execution. Without
// this invariant, adding two tiers or multiplying a fixed price by tokens
// would make both the request charge and its billing-unit trace ambiguous.
func validateFixedPricingTree(node ast.Node) error {
	switch part := node.(type) {
	case *ast.ConditionalNode:
		if containsPricingMarker(part.Cond) {
			break
		}
		if err := validateFixedPricingTree(part.Exp1); err != nil {
			return err
		}
		return validateFixedPricingTree(part.Exp2)
	case *ast.BinaryNode:
		if part.Operator != "*" {
			break
		}
		if isRequestPriceMultiplier(part.Right) {
			return validateFixedPricingTree(part.Left)
		}
		if isRequestPriceMultiplier(part.Left) {
			return validateFixedPricingTree(part.Right)
		}
	case *ast.CallNode:
		callee, ok := part.Callee.(*ast.IdentifierNode)
		if !ok || callee.Value != "tier" || len(part.Arguments) != 2 || containsPricingMarker(part.Arguments[0]) {
			break
		}
		price := part.Arguments[1]
		fixed, ok := price.(*ast.CallNode)
		if ok {
			function, direct := fixed.Callee.(*ast.IdentifierNode)
			if direct && function.Value == "fixed" && len(fixed.Arguments) == 1 {
				amount, literal := requestRuleNumber(fixed.Arguments[0])
				if literal && amount >= 0 && !math.IsNaN(amount) && !math.IsInf(amount*1_000_000, 0) {
					return nil
				}
				return fmt.Errorf("fixed price must be a finite, non-negative numeric literal with a finite v1 value")
			}
		}
		if !containsPricingMarker(price) {
			return nil
		}
	}
	return fmt.Errorf("fixed pricing requires tier(name, fixed(amount)) leaves, conditional tiers and request multipliers; token and fixed charges cannot be combined in one leaf")
}
