package languages

import (
	"regexp"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Store-factory action resolution (Zustand / Redux Toolkit / Pinia / MobX).
// In these libraries an action is declared inside a factory call and invoked
// indirectly — `useStore.getState().fetchUser()` or
// `const {fetchUser} = useStore.getState(); fetchUser()`. The extractor stamps
// each store-action node with Meta["store_factory"]=<binding> and resolves the
// indirect call against the same-file store binding (or hands a provenance-
// tagged placeholder to the store-factory synthesizer for cross-file binding).
// Shared by the JavaScript and TypeScript extractors.

// jsStoreFactoryCallNames is the set of factory callees that mark an object
// literal as a store definition. Matched on the callee's last identifier so
// `redux.createStore` and `create` both qualify. The name gate is what keeps
// an ordinary `ctx.set({...})` object from being mistaken for a store.
var jsStoreFactoryCallNames = map[string]bool{
	"create":            true, // zustand
	"createStore":       true, // redux / zustand vanilla
	"configureStore":    true, // redux toolkit
	"createSlice":       true, // redux toolkit
	"defineStore":       true, // pinia
	"makeAutoObservable": true, // mobx
	"makeObservable":    true, // mobx
	"observable":        true, // mobx
}

// jsGetStateChainRE recognises a store-accessor receiver:
// `useStore.getState()`, `.getStore()`, `.getActions()`, `.use()`.
var jsGetStateChainRE = regexp.MustCompile(`^(\w+)\.(?:getState|getStore|getActions|use)\(\)$`)

// jsDestructureGetStateRE recognises `const {a, b} = useStore.getState()` so a
// later bare `a()` / `b()` call binds to the store action.
var jsDestructureGetStateRE = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]*)\}\s*=\s*(\w+)\.(?:getState|getStore|getActions|use)\(\)`)

// jsStoreFactoryBinding reports whether the given object-literal member node is
// an action of a store-factory binding, returning the binding name and the
// factory callee. It walks up from the member through the (possibly
// middleware-wrapped) factory call to the variable_declarator that names the
// store, requiring a recognised factory callee along the way.
func jsStoreFactoryBinding(member *sitter.Node, src []byte) (binding, factory string, ok bool) {
	if member == nil {
		return "", "", false
	}
	saw := ""
	for cur := member.Parent(); cur != nil; cur = cur.Parent() {
		switch cur.Type() {
		case "call_expression":
			if fn := cur.ChildByFieldName("function"); fn != nil {
				if name := jsCalleeLastName(fn, src); jsStoreFactoryCallNames[name] {
					saw = name
				}
			}
		case "variable_declarator":
			if saw == "" {
				return "", "", false
			}
			if name := cur.ChildByFieldName("name"); name != nil && name.Type() == "identifier" {
				return name.Content(src), saw, true
			}
			return "", "", false
		case "program", "class_body":
			return "", "", false
		}
	}
	return "", "", false
}

// jsCalleeLastName returns the trailing identifier of a call's function
// expression: "create" for `create`, "createStore" for `redux.createStore`.
func jsCalleeLastName(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "member_expression":
		if p := fn.ChildByFieldName("property"); p != nil {
			return p.Content(src)
		}
	}
	return ""
}

// jsStoreOptionKeys are the object-literal keys under which Pinia and Redux
// Toolkit nest their action functions (`defineStore('id',{actions:{...}})`,
// `createSlice({reducers:{...}})`). When jsObjectOwnerName resolves a member's
// owner to one of these, the member is still a store action — the factory walk
// confirms it — so the extractor reruns store-factory detection for them.
var jsStoreOptionKeys = map[string]bool{
	"actions":  true, // pinia / vuex
	"reducers": true, // redux toolkit
	"methods":  true, // generic
}

// jsIsStoreOptionKey reports whether an owner name is a store option key.
func jsIsStoreOptionKey(owner string) bool { return jsStoreOptionKeys[owner] }

// jsParseGetStateChain extracts the store binding from a member-call receiver
// of the form `<binding>.getState()`.
func jsParseGetStateChain(receiver string) (binding string, ok bool) {
	m := jsGetStateChainRE.FindStringSubmatch(receiver)
	if m == nil {
		return "", false
	}
	return m[1], true
}
