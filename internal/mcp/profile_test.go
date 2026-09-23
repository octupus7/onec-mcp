package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/mcp-sales-mvp/internal/onec"
)

// capsFromJSON собирает профиль так же, как он приходит из 1С, — через JSON, а не через
// литерал структуры: тест заодно проверяет, что теги разбора совпадают с форматом 1С.
func capsFromJSON(t *testing.T, payload string) *onec.Capabilities {
	t.Helper()

	var caps onec.Capabilities
	if err := json.Unmarshal([]byte(payload), &caps); err != nil {
		t.Fatalf("failed to parse capabilities: %v", err)
	}

	return &caps
}

func findTool(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()

	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}

	t.Fatalf("tool %s not found", name)
	return Tool{}
}

func enumOf(t *testing.T, tool Tool, field string) []string {
	t.Helper()

	props := schemaProperties(tool)
	if props == nil {
		t.Fatalf("tool %s has no properties", tool.Name)
	}

	holder := enumHolder(props, field)
	if holder == nil {
		t.Fatalf("tool %s has no field %s", tool.Name, field)
	}

	values, ok := holder["enum"].([]string)
	if !ok {
		t.Fatalf("tool %s field %s has no enum", tool.Name, field)
	}

	return values
}

func hasValue(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func hasFilter(t *testing.T, tool Tool, name string) bool {
	t.Helper()

	props := schemaProperties(tool)
	filters, ok := props["filters"].(map[string]any)
	if !ok {
		t.Fatalf("tool %s has no filters", tool.Name)
	}

	filterProps, ok := filters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("tool %s filters have no properties", tool.Name)
	}

	_, found := filterProps[name]
	return found
}

// availableExcept — JSON-массив имён всех инструментов гейта, кроме перечисленных:
// профиль версии 2 перечисляет подтверждённое, а не скрытое.
func availableExcept(skip ...string) string {
	skipped := make(map[string]bool, len(skip))
	for _, name := range skip {
		skipped[name] = true
	}

	names := make([]string, 0, len(GetTools()))
	for _, tool := range GetTools() {
		if !skipped[tool.Name] {
			names = append(names, tool.Name)
		}
	}

	payload, _ := json.Marshal(names)
	return string(payload)
}

// uppProfile — профиль, который отдаёт УПП 1.3 (сокращённый до проверяемых граней).
var uppProfile = `{
	"profile": "upp-1.3",
	"version": 2,
	"unsupported": {
		"cash_flow": {"filters": ["cost_article_ids"]},
		"stock_balance": {"filters": ["firm_ids", "product_status"], "group_by": ["firm"]},
		"specification_explode": {"params": ["matrix_id", "composition_type_id", "production_group_id"]}
	},
	"extra": {
		"production_consumption": {"group_by": ["cost_article"]},
		"returns_report": {"group_by": ["sale_document"], "filters": ["sale_document_ids", "customer_ids", "unknown_ids"]}
	},
	"tools": {"available": ` + availableExcept(ToolAvailabilityReport, ToolGoodsInTransit) + `},
	"resolvers": {"always_empty": ["material"]}
}`

func TestApplyProfileStripsUnsupportedFacets(t *testing.T) {
	tools := applyProfile(GetTools(), capsFromJSON(t, uppProfile))

	if hasFilter(t, findTool(t, tools, ToolCashFlow), "cost_article_ids") {
		t.Error("cash_flow still offers cost_article_ids, which this database rejects with 400")
	}

	stock := findTool(t, tools, ToolStockBalance)

	if hasFilter(t, stock, "firm_ids") {
		t.Error("stock_balance still offers firm_ids")
	}

	if hasFilter(t, stock, "product_status") {
		t.Error("stock_balance still offers product_status")
	}

	if groups := enumOf(t, stock, "group_by"); hasValue(groups, "firm") {
		t.Errorf("stock_balance group_by still offers firm: %v", groups)
	}

	// Соседние грани не должны пострадать: вырезаем точечно, а не «всё про фирмы».
	if !hasFilter(t, stock, "warehouse_ids") {
		t.Error("stock_balance lost warehouse_ids, which is supported")
	}

	if !hasFilter(t, findTool(t, tools, ToolCashFlow), "operation_ids") {
		t.Error("cash_flow lost operation_ids, which is the supported alternative")
	}
}

func TestApplyProfileStripsParams(t *testing.T) {
	explode := findTool(t, applyProfile(GetTools(), capsFromJSON(t, uppProfile)), ToolSpecificationExplode)

	props := schemaProperties(explode)

	for _, name := range []string{"matrix_id", "composition_type_id", "production_group_id"} {
		if _, found := props[name]; found {
			t.Errorf("specification_explode still offers %s", name)
		}
	}

	if _, found := props["product_id"]; !found {
		t.Error("specification_explode lost product_id, which is required")
	}
}

// Параметр, вырезанный из properties, обязан исчезнуть и из required, иначе схема
// становится невыполнимой: модель не может передать то, чего в ней нет.
func TestApplyProfileKeepsRequiredConsistent(t *testing.T) {
	profile := `{"version": 2, "tools": {"available": ["specification_explode"]},
		"unsupported": {"specification_explode": {"params": ["product_id"]}}}`

	explode := findTool(t, applyProfile(GetTools(), capsFromJSON(t, profile)), ToolSpecificationExplode)

	schema, ok := explode.InputSchema.(map[string]any)
	if !ok {
		t.Fatal("specification_explode has no schema")
	}

	required, _ := schema["required"].([]string)
	if hasValue(required, "product_id") {
		t.Errorf("product_id stripped from properties but left in required: %v", required)
	}
}

func TestApplyProfileAddsExtraFacets(t *testing.T) {
	consumption := findTool(t, applyProfile(GetTools(), capsFromJSON(t, uppProfile)), ToolProductionConsumption)

	groups := enumOf(t, consumption, "group_by")

	if !hasValue(groups, "cost_article") {
		t.Errorf("production_consumption group_by missing cost_article, which this database supports: %v", groups)
	}

	// Добавление не должно вытеснять то, что уже было.
	if !hasValue(groups, "material") {
		t.Errorf("production_consumption group_by lost material: %v", groups)
	}
}

// extra.filters добавляет отбор-массив UUID; уже объявленный отбор остаётся как был.
func TestApplyProfileAddsExtraFilters(t *testing.T) {
	returns := findTool(t, applyProfile(GetTools(), capsFromJSON(t, uppProfile)), ToolReturnsReport)

	filterProps := schemaProperties(returns)["filters"].(map[string]any)["properties"].(map[string]any)

	added, ok := filterProps["sale_document_ids"].(map[string]any)
	if !ok {
		t.Fatalf("returns_report lacks sale_document_ids, which this database supports: %v", filterProps)
	}

	if added["type"] != "array" || added["items"].(map[string]any)["type"] != "string" {
		t.Errorf("sale_document_ids is not an array of strings: %v", added)
	}

	if description, _ := added["description"].(string); !strings.Contains(description, "find_document") {
		t.Errorf("sale_document_ids got no gate-side description: %q", description)
	}

	// Грань без описания в гейте всё равно добавляется — с общим текстом.
	if unknown, ok := filterProps["unknown_ids"].(map[string]any); !ok || unknown["description"] == "" {
		t.Errorf("unknown_ids not added with a fallback description: %v", filterProps["unknown_ids"])
	}

	customer := filterProps["customer_ids"].(map[string]any)
	if description, _ := customer["description"].(string); !strings.Contains(description, "Retail returns") {
		t.Errorf("customer_ids description overwritten by extra: %q", description)
	}

	// Пояснение к разрезу дописывается только для добавленного значения.
	groupBy := schemaProperties(returns)["group_by"].(map[string]any)["description"].(string)
	if !strings.Contains(groupBy, "sale_document =") {
		t.Errorf("group_by description lacks the sale_document note: %q", groupBy)
	}

	if strings.Contains(groupBy, "order =") {
		t.Errorf("group_by description explains order, which this profile did not add: %q", groupBy)
	}
}

// Без extra у базы нет ни отборов, ни пояснений о связи с продажей.
func TestReturnsSaleLinkHiddenWithoutProfile(t *testing.T) {
	returns := findTool(t, GetTools(), ToolReturnsReport)

	if hasFilter(t, returns, "sale_document_ids") || hasFilter(t, returns, "order_ids") {
		t.Error("returns_report offers sale-link filters in the common schema")
	}

	if groups := enumOf(t, returns, "group_by"); hasValue(groups, "sale_document") || hasValue(groups, "order") {
		t.Errorf("returns_report offers sale-link dimensions in the common schema: %v", groups)
	}
}

func TestApplyProfileDropsUnavailableTools(t *testing.T) {
	tools := applyProfile(GetTools(), capsFromJSON(t, uppProfile))

	for _, tool := range tools {
		if tool.Name == ToolAvailabilityReport || tool.Name == ToolGoodsInTransit {
			t.Errorf("tool %s is unavailable in this database but still listed", tool.Name)
		}
	}

	if len(tools) != len(GetTools())-2 {
		t.Errorf("expected exactly 2 tools dropped, got %d of %d", len(tools), len(GetTools()))
	}
}

// Суть opt-in: инструмент, которого база не назвала, не виден — в том числе появившийся
// в гейте позже, чем база обновила профиль.
func TestApplyProfileHidesUnconfirmedTools(t *testing.T) {
	profile := `{"version": 2, "tools": {"available": ["resolve_product", "stock_balance"]}}`

	tools := applyProfile(GetTools(), capsFromJSON(t, profile))

	if len(tools) != 2 {
		names := make([]string, 0, len(tools))
		for _, tool := range tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("expected only the 2 confirmed tools, got %v", names)
	}

	findTool(t, tools, ToolResolveProduct)
	findTool(t, tools, ToolStockBalance)
}

// База ответила, но ничего не подтвердила (пустой профиль, в который клиент превращает
// health без профиля или с незнакомой версией) — инструментов нет.
func TestApplyProfileEmptyHidesEverything(t *testing.T) {
	tools := applyProfile(GetTools(), capsFromJSON(t, `{"version": 2}`))

	if len(tools) != 0 {
		t.Errorf("a profile that confirms nothing still lists %d tools", len(tools))
	}
}

// Переходный период: профиль версии 1 перечисляет скрытое, и всё остальное остаётся видимым.
func TestApplyProfileLegacyVersion1(t *testing.T) {
	profile := `{"version": 1, "tools": {"unavailable": ["goods_in_transit"]}}`

	tools := applyProfile(GetTools(), capsFromJSON(t, profile))

	if len(tools) != len(GetTools())-1 {
		t.Errorf("version 1 profile: expected exactly 1 tool dropped, got %d of %d", len(tools), len(GetTools()))
	}

	for _, tool := range tools {
		if tool.Name == ToolGoodsInTransit {
			t.Error("goods_in_transit is unavailable in the version 1 profile but still listed")
		}
	}
}

// Резолвер, который всегда пуст, остаётся в списке: он работает, просто ничего не находит.
// Убрать его совсем — значит потерять инструмент из виду, когда база заполнит признак.
func TestApplyProfileMarksAlwaysEmptyResolver(t *testing.T) {
	material := findTool(t, applyProfile(GetTools(), capsFromJSON(t, uppProfile)), ToolResolveMaterial)

	if !strings.Contains(material.Description, "always returns an empty list") {
		t.Errorf("resolve_material description carries no warning: %q", material.Description)
	}

	product := findTool(t, applyProfile(GetTools(), capsFromJSON(t, uppProfile)), ToolResolveProduct)

	if strings.Contains(product.Description, "always returns an empty list") {
		t.Error("resolve_product wrongly marked as always empty")
	}
}

// nil — профиль неизвестен (1С недоступна и ещё ни разу его не отдала). Недоступность 1С
// не должна сужать выдачу: сессия, открытая в момент сбоя, осталась бы без инструментов.
func TestApplyProfileNilIsNoop(t *testing.T) {
	tools := applyProfile(GetTools(), nil)

	if len(tools) != len(GetTools()) {
		t.Errorf("nil profile changed the tool count: %d vs %d", len(tools), len(GetTools()))
	}

	if !hasFilter(t, findTool(t, tools, ToolCashFlow), "cost_article_ids") {
		t.Error("nil profile stripped cost_article_ids")
	}
}

// Профиль не должен пережить вызов: GetTools() строит схемы заново, и правка одной
// сессии не может протечь в следующую.
func TestApplyProfileDoesNotLeakIntoNextCall(t *testing.T) {
	applyProfile(GetTools(), capsFromJSON(t, uppProfile))

	if !hasFilter(t, findTool(t, GetTools(), ToolCashFlow), "cost_article_ids") {
		t.Error("applyProfile mutated shared schema state: cost_article_ids gone from a fresh GetTools()")
	}
}

// Профиль, снятый с живой базы УПП 1.3 (GET /mcp/health). Держим его фикстурой, чтобы
// расхождение имён между сторонами ловилось тестом, а не поведением в проде: ключи профиля
// — имена инструментов гейта, и опечатка в 1С (specification вместо product_specification)
// иначе прошла бы незамеченной — профиль просто ничего бы не вырезал.
func realUppProfile(t *testing.T) *onec.Capabilities {
	t.Helper()

	payload, err := os.ReadFile(filepath.Join("testdata", "upp_profile.json"))
	if err != nil {
		t.Fatalf("failed to read the profile fixture: %v", err)
	}

	return capsFromJSON(t, string(payload))
}

// Каждый ключ профиля должен попадать в существующий инструмент. Ключ, не совпавший ни с
// чем, — это молчаливая опечатка: гейт не упадёт, просто не сделает того, о чём его просят.
func TestRealProfileNamesMatchTools(t *testing.T) {
	caps := realUppProfile(t)

	known := make(map[string]bool)
	for _, tool := range GetTools() {
		known[tool.Name] = true
	}

	for name := range caps.Unsupported {
		if !known[name] {
			t.Errorf("unsupported names %q, which is not a tool of this gate", name)
		}
	}

	for name := range caps.Extra {
		if !known[name] {
			t.Errorf("extra names %q, which is not a tool of this gate", name)
		}
	}

	for _, name := range caps.Tools.Available {
		if !known[name] {
			t.Errorf("tools.available names %q, which is not a tool of this gate", name)
		}
	}

	for _, entity := range caps.Resolvers.AlwaysEmpty {
		if !known["resolve_"+entity] {
			t.Errorf("resolvers.always_empty names %q, but resolve_%s does not exist", entity, entity)
		}
	}
}

// Сквозная проверка на реальном профиле: то, что база отклоняет 400-ми, не должно доезжать
// до модели.
func TestRealProfileShapesTools(t *testing.T) {
	tools := applyProfile(GetTools(), realUppProfile(t))

	if hasFilter(t, findTool(t, tools, ToolCashFlow), "cost_article_ids") {
		t.Error("cash_flow still offers cost_article_ids")
	}

	if groups := enumOf(t, findTool(t, tools, ToolSalesReport), "group_by"); hasValue(groups, "sales_channel") {
		t.Errorf("sales_report group_by still offers sales_channel: %v", groups)
	}

	spec := findTool(t, tools, ToolProductSpecification)
	if _, found := schemaProperties(spec)["matrix_id"]; found {
		t.Error("product_specification still offers matrix_id")
	}

	if groups := enumOf(t, findTool(t, tools, ToolProductionConsumption), "group_by"); !hasValue(groups, "cost_article") {
		t.Errorf("production_consumption group_by lacks cost_article: %v", groups)
	}

	// Закупки в этой базе считаются по регистру, где нет ни склада, ни валюты, ни
	// признака «в пути», зато есть договор и подразделение.
	purchases := findTool(t, tools, ToolPurchasesReport)

	if hasFilter(t, purchases, "warehouse_ids") {
		t.Error("purchases_report still offers warehouse_ids")
	}

	if _, found := schemaProperties(purchases)["in_transit"]; found {
		t.Error("purchases_report still offers in_transit")
	}

	purchaseGroups := enumOf(t, purchases, "group_by")

	for _, gone := range []string{"warehouse", "currency", "in_transit", "delivery_date"} {
		if hasValue(purchaseGroups, gone) {
			t.Errorf("purchases_report group_by still offers %s: %v", gone, purchaseGroups)
		}
	}

	for _, added := range []string{"contract", "department", "project", "document"} {
		if !hasValue(purchaseGroups, added) {
			t.Errorf("purchases_report group_by lacks %s, which this database supports: %v", added, purchaseGroups)
		}
	}

	if measures := enumOf(t, purchases, "measures"); hasValue(measures, "amount_currency") {
		t.Errorf("purchases_report measures still offer amount_currency: %v", measures)
	}

	// Имя в профиле — имя ИНСТРУМЕНТА гейта, а не типа отчёта 1С: строка "reserves" вместо
	// "stock_reserves" не спрятала бы ничего, и модель получала бы от базы 400.
	for _, name := range []string{ToolAvailabilityReport, ToolGoodsInTransit, ToolProductDetails, ToolStockReserves} {
		for _, tool := range tools {
			if tool.Name == name {
				t.Errorf("tool %s is not implemented in this database but is still listed", name)
			}
		}
	}

	returns := findTool(t, tools, ToolReturnsReport)

	for _, name := range []string{"order_ids", "sale_document_ids"} {
		if !hasFilter(t, returns, name) {
			t.Errorf("returns_report lacks filter %s, which this database supports", name)
		}
	}

	if groups := enumOf(t, returns, "group_by"); !hasValue(groups, "order") || !hasValue(groups, "sale_document") {
		t.Errorf("returns_report group_by lacks order / sale_document: %v", groups)
	}

	if !strings.Contains(findTool(t, tools, ToolResolveMaterial).Description, "always returns an empty list") {
		t.Error("resolve_material carries no empty-result warning")
	}
}
