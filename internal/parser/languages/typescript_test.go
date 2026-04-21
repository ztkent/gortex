package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestTSExtractor_Function(t *testing.T) {
	src := []byte(`function greet(name: string): string {
  return "Hello " + name;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "greet", funcs[0].Name)
}

func TestTSExtractor_ArrowFunction(t *testing.T) {
	src := []byte(`const handler = () => {
  console.log("hello");
};
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "handler", funcs[0].Name)
}

func TestTSExtractor_Class(t *testing.T) {
	src := []byte(`class UserService {
  getUser(id: string) {
    return {};
  }
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("service.ts", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "getUser", methods[0].Name)
}

func TestTSExtractor_Interface(t *testing.T) {
	src := []byte(`interface Config {
  port: number;
  host: string;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("types.ts", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "Config", ifaces[0].Name)
}

func TestTSExtractor_Variables(t *testing.T) {
	src := []byte(`const API_URL = "https://api.example.com";
let count = 0;
export const MAX_RETRIES = 3;
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("config.ts", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.GreaterOrEqual(t, len(vars), 2)
}

func TestTSExtractor_Enum(t *testing.T) {
	src := []byte(`export enum KeybindingWeight {
    EditorCore = 0,
    EditorContrib = 100,
    WorkbenchContrib = 200,
    BuiltinExtension = 300,
    ExternalExtension = 400
}

enum Simple {
    A,
    B,
    C
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("weights.ts", src)
	require.NoError(t, err)

	// Enums come through as KindType with Meta["kind"]="enum".
	enumNames := map[string]bool{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindType && n.Meta != nil && n.Meta["kind"] == "enum" {
			enumNames[n.Name] = true
		}
	}
	assert.Equal(t, map[string]bool{"KeybindingWeight": true, "Simple": true}, enumNames)

	// Members are KindVariable with Meta["kind"]="enum_member".
	memberCount := 0
	byReceiver := map[string]int{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindVariable && n.Meta != nil && n.Meta["kind"] == "enum_member" {
			memberCount++
			if recv, ok := n.Meta["receiver"].(string); ok {
				byReceiver[recv]++
			}
		}
	}
	assert.Equal(t, 8, memberCount) // 5 + 3
	assert.Equal(t, 5, byReceiver["KeybindingWeight"])
	assert.Equal(t, 3, byReceiver["Simple"])
}

func TestTSExtractor_ClassProperties(t *testing.T) {
	src := []byte(`class Server {
    public readonly port: number = 8080;
    private _connections: number = 0;
    protected logger: Logger;

    start() {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("server.ts", src)
	require.NoError(t, err)

	props := map[string]bool{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindVariable && n.Meta != nil && n.Meta["kind"] == "class_property" {
			props[n.Name] = true
		}
	}
	assert.Equal(t, map[string]bool{"port": true, "_connections": true, "logger": true}, props)
}

func TestTSExtractor_InterfaceMethods(t *testing.T) {
	src := []byte(`interface Repository {
    findById(id: string): User;
    save(user: User): void;
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("repo.ts", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	methods, ok := ifaces[0].Meta["methods"].([]string)
	require.True(t, ok)
	assert.Contains(t, methods, "findById")
	assert.Contains(t, methods, "save")
}

func TestTSExtractor_Imports(t *testing.T) {
	src := []byte(`import { Router } from 'express';
import axios from 'axios';
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 2)
}

func TestTSExtractor_TypeEnv_ExplicitType(t *testing.T) {
	src := []byte(`
class UserService {
  save() {}
}

function main() {
  const svc: UserService = new UserService();
  svc.save();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on save call edge")
	assert.Equal(t, "UserService", saveCall.Meta["receiver_type"])
}

func TestTSExtractor_TypeEnv_NewExpression(t *testing.T) {
	src := []byte(`
class Client {
  connect() {}
}

function main() {
  const client = new Client();
  client.connect();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var connectCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "connect") {
			connectCall = c
			break
		}
	}
	require.NotNil(t, connectCall)
	require.NotNil(t, connectCall.Meta)
	assert.Equal(t, "Client", connectCall.Meta["receiver_type"])
}

func TestTSExtractor_TypeEnv_Unknown(t *testing.T) {
	src := []byte(`
function getService() { return null; }

function main() {
  const svc = getService();
  svc.process();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var processCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "process") {
			processCall = c
			break
		}
	}
	require.NotNil(t, processCall)
	assert.Nil(t, processCall.Meta, "unknown type should not produce Meta")
}

func TestTSExtractor_TypeEnv_Chain(t *testing.T) {
	src := []byte(`
class Connection {
  query(): Result {
    return new Result();
  }
}

class Result {
  first(): User {
    return new User();
  }
}

class User {
  save() {}
}

function main() {
  const conn = new Connection();
  conn.query().first().save();
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("app.ts", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on chained save call edge")
	assert.Equal(t, "User", saveCall.Meta["receiver_type"])
}

func TestTSExtractor_MethodReceiver(t *testing.T) {
	src := []byte(`
class Server {
  start() {}
  stop() {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("server.ts", src)
	require.NoError(t, err)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	for _, m := range methods {
		assert.Equal(t, "Server", m.Meta["receiver"], "method %s should have receiver Server", m.Name)
	}
}

func TestTSExtractor_NestJsUseGuardsDispatch(t *testing.T) {
	// @UseGuards on a controller method should emit a synthetic call edge
	// from the handler to the guard's canActivate method. This is the one
	// DI shape that has no explicit call site anywhere in source — the
	// framework dispatches to the guard based on decorator metadata.
	src := []byte(`
import { Controller, Post, UseGuards } from '@nestjs/common';
import { AuthGuard } from './auth.guard';

@Controller('x')
export class XController {
  @Post()
  @UseGuards(AuthGuard)
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	var dispatch *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if d, ok := ed.Meta["dispatch_decorator"].(string); ok && d == "UseGuards" {
			dispatch = ed
			break
		}
	}
	require.NotNil(t, dispatch, "expected a dispatch edge tagged UseGuards")
	assert.Equal(t, "x.controller.ts::XController.handle", dispatch.From)
	assert.Equal(t, "unresolved::*.canActivate", dispatch.To)
	assert.Equal(t, "AuthGuard", dispatch.Meta["receiver_type"])
}

func TestTSExtractor_NestJsUseInterceptorsDispatch(t *testing.T) {
	// Same shape for @UseInterceptors → intercept.
	src := []byte(`
import { Controller, Get, UseInterceptors } from '@nestjs/common';
import { CacheInterceptor } from './cache.interceptor';

@Controller('x')
export class XController {
  @Get()
  @UseInterceptors(CacheInterceptor)
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	var dispatch *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if d, ok := ed.Meta["dispatch_decorator"].(string); ok && d == "UseInterceptors" {
			dispatch = ed
			break
		}
	}
	require.NotNil(t, dispatch)
	assert.Equal(t, "unresolved::*.intercept", dispatch.To)
	assert.Equal(t, "CacheInterceptor", dispatch.Meta["receiver_type"])
}

func TestTSExtractor_NestJsMultipleGuards(t *testing.T) {
	// @UseGuards(A, B) must emit one edge per class argument.
	src := []byte(`
import { Controller, Get, UseGuards } from '@nestjs/common';

@Controller('x')
export class XController {
  @Get()
  @UseGuards(Auth, Role)
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	var count int
	seen := map[string]bool{}
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if d, _ := ed.Meta["dispatch_decorator"].(string); d == "UseGuards" {
			count++
			seen[ed.Meta["receiver_type"].(string)] = true
		}
	}
	assert.Equal(t, 2, count, "expected one edge per guard class")
	assert.True(t, seen["Auth"] && seen["Role"])
}

func TestTSExtractor_NestJsNonDispatchDecoratorIgnored(t *testing.T) {
	// @Post / @Get / @Injectable / custom decorators must not produce
	// dispatch edges — only the explicit @Use* set above does.
	src := []byte(`
import { Controller, Get, Post } from '@nestjs/common';

@Controller('x')
export class XController {
  @Get()
  @Post('send')
  async handle(): Promise<void> {}
}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("x.controller.ts", src)
	require.NoError(t, err)

	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		if _, ok := ed.Meta["dispatch_decorator"]; ok {
			t.Fatalf("unexpected dispatch edge: %+v", ed)
		}
	}
}

func TestTSExtractor_NestJsModuleUseClassBinding(t *testing.T) {
	// @Module({ providers: [{ provide: Abstract, useClass: Concrete }] })
	// should produce a Provides edge from the module to Concrete, tagged
	// provides_for: Abstract, so the resolver can pick Concrete for
	// receiver_type=Abstract calls.
	src := []byte(`
import { Module } from '@nestjs/common';
import { Notifier } from './notifier';
import { EmailNotifier } from './email';

@Module({
  providers: [
    { provide: Notifier, useClass: EmailNotifier },
  ],
})
export class NotificationsModule {}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("notif.module.ts", src)
	require.NoError(t, err)

	var binding *graph.Edge
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		if b, _ := ed.Meta["binding"].(string); b == "useClass" {
			binding = ed
			break
		}
	}
	require.NotNil(t, binding, "expected a useClass EdgeProvides")
	assert.Equal(t, "notif.module.ts::NotificationsModule", binding.From)
	assert.Equal(t, "Notifier", binding.Meta["provides_for"])
	assert.Contains(t, binding.To, "EmailNotifier")
}

func TestTSExtractor_NestJsModuleSkipsNonUseClass(t *testing.T) {
	// useValue / bare-class providers must not produce useClass edges —
	// those are the @Inject(TOKEN) feature's territory, handled separately.
	src := []byte(`
import { Module } from '@nestjs/common';

@Module({
  providers: [
    { provide: 'TOKEN', useValue: 42 },
    BareService,
  ],
})
export class M {}
`)
	e := NewTypeScriptExtractor()
	result, err := e.Extract("m.ts", src)
	require.NoError(t, err)

	for _, ed := range edgesOfKind(result.Edges, graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		if b, _ := ed.Meta["binding"].(string); b == "useClass" {
			t.Fatalf("unexpected useClass edge: %+v", ed)
		}
	}
}
