package ledger

func ClassifyConcurrentWriters(delta map[string]any, delegatedScope, materialPaths, verificationPaths []string, protectedPaths *[]string) map[string]any {
	unknown := func(code string) map[string]any {
		return map[string]any{"decision": "UNKNOWN_CONCURRENT_WRITER", "code": code, "concurrent_writer": nil,
			"in_scope_paths": nil, "out_of_scope_paths": nil, "review_changed_file_count": nil,
			"review_changed_line_count": nil}
	}
	if delta == nil || delta["status"] != OK {
		return unknown("UNKNOWN_DELTA")
	}
	scope, err := normalizeScope(delegatedScope)
	if err != nil {
		return unknown(err.(*inspectionError).Code)
	}
	material, verification := []string{}, []string{}
	if len(materialPaths) > 0 {
		material, err = normalizeScope(materialPaths)
		if err != nil {
			return unknown(err.(*inspectionError).Code)
		}
	}
	if len(verificationPaths) > 0 {
		verification, err = normalizeScope(verificationPaths)
		if err != nil {
			return unknown(err.(*inspectionError).Code)
		}
	}
	changed, _ := stringList(delta["changed_paths"])
	inScopeSet := map[string]bool{}
	for _, path := range changed {
		if pathInScope(path, scope) {
			inScopeSet[path] = true
		}
	}
	inScope := sortedKeys(inScopeSet)
	inScopeLookup := inScopeSet
	outsideSet := map[string]bool{}
	for _, path := range changed {
		if !inScopeLookup[path] {
			outsideSet[path] = true
		}
	}
	outside := sortedKeys(outsideSet)
	preexisting, _ := stringList(delta["preexisting_paths"])
	preserved := map[string]bool{}
	for _, path := range append(append([]string{}, outside...), preexisting...) {
		if !pathInScope(path, scope) {
			preserved[path] = true
		}
	}
	ambiguous, _ := stringList(delta["ambiguous_paths"])
	ambiguousSet, overlapSet := map[string]bool{}, map[string]bool{}
	for _, path := range ambiguous {
		ambiguousSet[path] = true
		if inScopeLookup[path] {
			overlapSet[path] = true
		}
	}
	boundary := func(paths []string) []string {
		found := map[string]bool{}
		for _, path := range outside {
			for _, item := range paths {
				if pathInScope(path, []string{item}) || pathInScope(item, []string{path}) {
					found[path] = true
					break
				}
			}
		}
		return sortedKeys(found)
	}
	materialTouches, verificationTouches := boundary(material), boundary(verification)
	protected := DetectProtectedPaths(changed, protectedPaths)
	haltSet := map[string]bool{}
	for _, paths := range [][]string{sortedKeys(overlapSet), materialTouches, verificationTouches, protected, ambiguous} {
		for _, path := range paths {
			haltSet[path] = true
		}
	}
	halt := sortedKeys(haltSet)
	decision := "PROCEED"
	if len(halt) > 0 {
		decision = "UNKNOWN_CONCURRENT_WRITER"
	}
	counts, _ := countMap(delta["path_line_counts"])
	lines := 0
	for _, path := range inScope {
		lines += counts[path]
	}
	return map[string]any{"decision": decision, "code": decision, "concurrent_writer": len(outside) > 0,
		"in_scope_paths": inScope, "out_of_scope_paths": outside, "preserved_out_of_scope_paths": sortedKeys(preserved),
		"overlap_paths": sortedKeys(overlapSet), "material_boundary_paths": materialTouches,
		"verification_boundary_paths": verificationTouches, "protected_paths": protected, "ambiguous_paths": sortedKeys(ambiguousSet),
		"halt_paths": halt, "review_changed_file_count": len(inScope), "review_changed_line_count": lines}
}
