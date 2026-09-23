package mcp

import (
	"strings"

	"example.com/mcp-sales-mvp/internal/onec"
)

// applyProfile правит список инструментов под профиль конкретной базы 1С.
//
// Зачем: общий контракт гейта шире любой отдельной базы, а различия порождает учётная
// модель. Без профиля модель видит в схеме фильтр, которого в этой базе нет, зовёт его и
// получает 400 — целый ход впустую, выглядящий как поломка. Профиль убирает такое из
// схемы до того, как модель это увидит.
//
// Приём тот же, что у stripCostMeasures: мутируем map'ы схемы на месте. Это безопасно,
// потому что GetTools() конструирует их заново на каждый запрос.
//
// Набор инструментов — opt-in (профиль версии 2): остаются только подтверждённые базой.
// Новый инструмент гейта не появляется ни у одной базы, пока та его не объявит.
//
// Отказы на стороне 1С остаются последней линией: гейт может быть старой версии или не
// достучаться до health, и тогда вызов должен упереться в понятную ошибку, а не в тишину.
func applyProfile(tools []Tool, caps *onec.Capabilities) []Tool {
	if caps == nil {
		return tools
	}

	// Резолвер, который в этой базе всегда пуст, из списка НЕ убирается: инструмент
	// работает и отвечает корректно, просто ничего не находит. Вместо этого правится
	// описание — модель не тратит на него ход, но и не теряет его из виду, когда база
	// наконец заполнит нужный признак.
	alwaysEmpty := make(map[string]bool, len(caps.Resolvers.AlwaysEmpty))
	for _, entity := range caps.Resolvers.AlwaysEmpty {
		alwaysEmpty["resolve_"+entity] = true
	}

	result := make([]Tool, 0, len(tools))

	for _, t := range tools {
		if !caps.ToolAvailable(t.Name) {
			continue
		}

		if facets, ok := caps.Unsupported[t.Name]; ok {
			stripFacets(t, facets)
		}

		if facets, ok := caps.Extra[t.Name]; ok {
			addFacets(t, facets, extraFacetNotes[t.Name])
		}

		if alwaysEmpty[t.Name] {
			t.Description = strings.TrimSpace(t.Description) +
				" NOTE: in this database this resolver always returns an empty list —" +
				" the underlying catalog or attribute does not exist here."
		}

		result = append(result, t)
	}

	return result
}

// stripFacets убирает из схемы инструмента то, чего база не поддерживает.
func stripFacets(t Tool, facets onec.SchemaFacets) {
	props := schemaProperties(t)
	if props == nil {
		return
	}

	for _, name := range facets.Params {
		delete(props, name)
		removeRequired(t, name)
	}

	if filters, ok := props["filters"].(map[string]any); ok {
		if filterProps, ok := filters["properties"].(map[string]any); ok {
			for _, name := range facets.Filters {
				delete(filterProps, name)
			}
		}
	}

	stripEnum(props, "group_by", facets.GroupBy)
	stripEnum(props, "measures", facets.Measures)
}

// addFacets досыпает в схему то, что база умеет, а общий контракт не объявляет.
// Обратная сторона профиля: молчаливо спрятанная возможность — та же ошибка, что
// обещанная несуществующая, просто тише.
//
// Профиль передаёт только имена; смысл грани модели объясняет notes — текст, известный гейту.
// Без него фильтр получает общее описание, а значение group_by — никакого.
func addFacets(t Tool, facets onec.SchemaFacets, notes facetNotes) {
	props := schemaProperties(t)
	if props == nil {
		return
	}

	if added := addEnum(props, "group_by", facets.GroupBy); len(added) > 0 {
		appendDescription(props, "group_by", notesFor(notes.GroupBy, added))
	}

	addEnum(props, "measures", facets.Measures)
	addFilters(props, facets.Filters, notes.Filters)
}

// addFilters добавляет отборы-массивы UUID. Уже объявленный отбор не перезаписывается:
// описание общей схемы точнее того, что гейт может сказать по одному имени.
func addFilters(props map[string]any, names []string, notes map[string]string) {
	if len(names) == 0 {
		return
	}

	filters, ok := props["filters"].(map[string]any)
	if !ok {
		return
	}

	filterProps, ok := filters["properties"].(map[string]any)
	if !ok {
		return
	}

	for _, name := range names {
		if _, exists := filterProps[name]; exists {
			continue
		}

		description := notes[name]
		if description == "" {
			description = "Filter by " + name + " (UUIDs)."
		}

		filterProps[name] = map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": description,
		}
	}
}

func notesFor(notes map[string]string, values []string) []string {
	result := make([]string, 0, len(values))
	for _, v := range values {
		if note := notes[v]; note != "" {
			result = append(result, note)
		}
	}

	return result
}

func appendDescription(props map[string]any, field string, notes []string) {
	if len(notes) == 0 {
		return
	}

	spec, ok := props[field].(map[string]any)
	if !ok {
		return
	}

	description, _ := spec["description"].(string)
	spec["description"] = strings.TrimSpace(description + " " + strings.Join(notes, " "))
}

func schemaProperties(t Tool) map[string]any {
	schema, ok := t.InputSchema.(map[string]any)
	if !ok {
		return nil
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}

	return props
}

// enumHolder возвращает map, в котором лежит enum поля: у массива это items,
// у скалярного поля — оно само.
func enumHolder(props map[string]any, field string) map[string]any {
	spec, ok := props[field].(map[string]any)
	if !ok {
		return nil
	}

	if items, ok := spec["items"].(map[string]any); ok {
		return items
	}

	return spec
}

func stripEnum(props map[string]any, field string, values []string) {
	if len(values) == 0 {
		return
	}

	holder := enumHolder(props, field)
	if holder == nil {
		return
	}

	current, ok := holder["enum"].([]string)
	if !ok {
		return
	}

	blocked := make(map[string]bool, len(values))
	for _, v := range values {
		blocked[v] = true
	}

	kept := make([]string, 0, len(current))
	for _, v := range current {
		if !blocked[v] {
			kept = append(kept, v)
		}
	}

	holder["enum"] = kept
}

// addEnum дописывает значения в enum и возвращает те, которых там ещё не было.
func addEnum(props map[string]any, field string, values []string) []string {
	if len(values) == 0 {
		return nil
	}

	holder := enumHolder(props, field)
	if holder == nil {
		return nil
	}

	current, ok := holder["enum"].([]string)
	if !ok {
		return nil
	}

	present := make(map[string]bool, len(current))
	for _, v := range current {
		present[v] = true
	}

	var added []string

	for _, v := range values {
		if !present[v] {
			current = append(current, v)
			added = append(added, v)
			present[v] = true
		}
	}

	holder["enum"] = current

	return added
}

// removeRequired вычёркивает параметр из required: оставленный там параметр, которого
// больше нет в properties, делает схему невалидной.
func removeRequired(t Tool, name string) {
	schema, ok := t.InputSchema.(map[string]any)
	if !ok {
		return
	}

	required, ok := schema["required"].([]string)
	if !ok {
		return
	}

	kept := make([]string, 0, len(required))
	for _, v := range required {
		if v != name {
			kept = append(kept, v)
		}
	}

	schema["required"] = kept
}
