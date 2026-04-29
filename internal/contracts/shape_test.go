package contracts

import (
	"testing"
)

// -----------------------------------------------------------------------------
// Go
// -----------------------------------------------------------------------------

func TestShape_Go_StructFields(t *testing.T) {
	src := []byte(`package api

type LoginReq struct {
	Email    string   ` + "`json:\"email\"`" + `
	Password string   ` + "`json:\"password,omitempty\"`" + `
	Profile  *Profile
	Tags     []string ` + "`json:\"tags\"`" + `
	skip     string   // unexported
	Ignored  string   ` + "`json:\"-\"`" + `
}
`)
	s := ExtractShape("pkg/req.go", src, 3, 10)
	if s == nil {
		t.Fatal("expected shape, got nil")
		return
	}
	if s.Kind != "struct" {
		t.Errorf("kind = %q, want struct", s.Kind)
	}
	want := map[string]ShapeField{
		"email":    {Name: "email", Type: "string", JSONTag: `json:"email"`, Required: true},
		"password": {Name: "password", Type: "string", JSONTag: `json:"password,omitempty"`, Required: false},
		"Profile":  {Name: "Profile", Type: "Profile", Required: false},
		"tags":     {Name: "tags", Type: "string", JSONTag: `json:"tags"`, Required: true, Repeated: true},
		"Ignored":  {Name: "Ignored", Type: "string", JSONTag: `json:"-"`, Required: false},
	}
	assertShapeFields(t, s, want)
}

// -----------------------------------------------------------------------------
// TypeScript
// -----------------------------------------------------------------------------

func TestShape_TS_InterfaceAndOptional(t *testing.T) {
	src := []byte(`export interface LoginReq {
  email: string
  password?: string
  tags: string[]
  profile: Profile | null
  readonly version: number
}
`)
	s := ExtractShape("pkg/types.ts", src, 1, 7)
	if s == nil {
		t.Fatal("expected shape, got nil")
		return
	}
	if s.Kind != "interface" {
		t.Errorf("kind = %q, want interface", s.Kind)
	}
	want := map[string]ShapeField{
		"email":    {Name: "email", Type: "string", Required: true},
		"password": {Name: "password", Type: "string", Required: false},
		"tags":     {Name: "tags", Type: "string[]", Required: true, Repeated: true},
		"profile":  {Name: "profile", Type: "Profile | null", Required: false},
		"version":  {Name: "version", Type: "number", Required: true},
	}
	assertShapeFields(t, s, want)
}

// -----------------------------------------------------------------------------
// Python
// -----------------------------------------------------------------------------

func TestShape_Python_PydanticClass(t *testing.T) {
	src := []byte(`class LoginReq(BaseModel):
    email: str
    password: str | None = None
    tags: list[str] = []
    profile: Profile
    alias_field: str = Field(alias="aliasField")
`)
	s := ExtractShape("pkg/models.py", src, 1, 6)
	if s == nil {
		t.Fatal("expected shape, got nil")
		return
	}
	if s.Kind != "class" {
		t.Errorf("kind = %q, want class", s.Kind)
	}
	want := map[string]ShapeField{
		"email":      {Name: "email", Type: "str", Required: true},
		"password":   {Name: "password", Type: "str | None", Required: false},
		"tags":       {Name: "tags", Type: "list[str]", Required: false, Repeated: true},
		"profile":    {Name: "profile", Type: "Profile", Required: true},
		"aliasField": {Name: "aliasField", Type: "str", JSONTag: "aliasField", Required: false},
	}
	assertShapeFields(t, s, want)
}

// -----------------------------------------------------------------------------
// Java
// -----------------------------------------------------------------------------

func TestShape_Java_ClassWithJacksonAnnotations(t *testing.T) {
	src := []byte(`public class LoginReq {
    @JsonProperty("email")
    private String email;

    @Nullable
    @JsonProperty("pw")
    private String password;

    private List<String> tags;
}
`)
	s := ExtractShape("pkg/LoginReq.java", src, 1, 10)
	if s == nil {
		t.Fatal("expected shape, got nil")
		return
	}
	want := map[string]ShapeField{
		"email": {Name: "email", Type: "String", JSONTag: "email", Required: true},
		"pw":    {Name: "pw", Type: "String", JSONTag: "pw", Required: false},
		"tags":  {Name: "tags", Type: "List", Required: true, Repeated: true},
	}
	assertShapeFields(t, s, want)
}

// -----------------------------------------------------------------------------
// Dart
// -----------------------------------------------------------------------------

func TestShape_Dart_ClassWithFields(t *testing.T) {
	src := []byte(`class EmailIngestLogEntry {
  final String id;
  final String createdAt;
  final String? provider;
  final List<String> tags;

  @JsonKey(name: 'user_id')
  final String userId;

  EmailIngestLogEntry(this.id, this.createdAt, this.provider, this.tags, this.userId);

  factory EmailIngestLogEntry.fromJson(Map<String, dynamic> j) => EmailIngestLogEntry(
    j['id'] as String,
    j['createdAt'] as String,
    j['provider'] as String?,
    (j['tags'] as List).cast<String>(),
    j['user_id'] as String,
  );
}
`)
	s := ExtractShape("lib/models/entry.dart", src, 1, 19)
	if s == nil {
		t.Fatal("expected shape, got nil")
		return
	}
	if s.Kind != "class" {
		t.Errorf("kind = %q, want class", s.Kind)
	}
	want := map[string]ShapeField{
		"id":        {Name: "id", Type: "String", Required: true},
		"createdAt": {Name: "createdAt", Type: "String", Required: true},
		"provider":  {Name: "provider", Type: "String", Required: false},
		"tags":      {Name: "tags", Type: "List", Required: true, Repeated: true},
		"user_id":   {Name: "user_id", Type: "String", JSONTag: "user_id", Required: true},
	}
	assertShapeFields(t, s, want)
}

// -----------------------------------------------------------------------------
// Proto
// -----------------------------------------------------------------------------

func TestShape_Proto_MessageFields(t *testing.T) {
	src := []byte(`message LoginReq {
  string email = 1;
  optional string password = 2;
  repeated string tags = 3;
  Profile profile = 4 [json_name = "userProfile"];
  map<string, Foo> items = 5;
}
`)
	s := ExtractShape("proto/login.proto", src, 1, 7)
	if s == nil {
		t.Fatal("expected shape, got nil")
		return
	}
	if s.Kind != "message" {
		t.Errorf("kind = %q, want message", s.Kind)
	}
	want := map[string]ShapeField{
		"email":       {Name: "email", Type: "string", Required: true},
		"password":    {Name: "password", Type: "string", Required: false},
		"tags":        {Name: "tags", Type: "string", Required: true, Repeated: true},
		"userProfile": {Name: "userProfile", Type: "Profile", JSONTag: "userProfile", Required: true},
		"items":       {Name: "items", Type: "map<string, Foo>", Required: true, Repeated: true},
	}
	assertShapeFields(t, s, want)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func assertShapeFields(t *testing.T, s *Shape, want map[string]ShapeField) {
	t.Helper()
	got := make(map[string]ShapeField, len(s.Fields))
	for _, f := range s.Fields {
		got[f.Name] = f
	}
	for name, wf := range want {
		gf, ok := got[name]
		if !ok {
			t.Errorf("missing field %q (have %v)", name, fieldNames(s.Fields))
			continue
		}
		if gf.Type != wf.Type {
			t.Errorf("%s.Type = %q, want %q", name, gf.Type, wf.Type)
		}
		if gf.Required != wf.Required {
			t.Errorf("%s.Required = %v, want %v", name, gf.Required, wf.Required)
		}
		if gf.Repeated != wf.Repeated {
			t.Errorf("%s.Repeated = %v, want %v", name, gf.Repeated, wf.Repeated)
		}
		if wf.JSONTag != "" && gf.JSONTag != wf.JSONTag {
			t.Errorf("%s.JSONTag = %q, want %q", name, gf.JSONTag, wf.JSONTag)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected field %q", name)
		}
	}
}

func fieldNames(fs []ShapeField) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}
