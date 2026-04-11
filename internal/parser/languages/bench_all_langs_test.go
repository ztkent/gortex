package languages

import (
	"testing"
)

// =============================================================================
// Source samples for all remaining languages not covered in bench_test.go.
// Existing: Go, TypeScript, Python, Rust, Java, Ruby, Elixir
// =============================================================================

var cSource = []byte(`#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    char *key;
    char *value;
} Entry;

typedef struct {
    Entry *entries;
    int size;
    int capacity;
} HashMap;

HashMap *hashmap_new(int capacity) {
    HashMap *map = malloc(sizeof(HashMap));
    map->entries = calloc(capacity, sizeof(Entry));
    map->size = 0;
    map->capacity = capacity;
    return map;
}

void hashmap_put(HashMap *map, const char *key, const char *value) {
    if (map->size >= map->capacity) return;
    map->entries[map->size].key = strdup(key);
    map->entries[map->size].value = strdup(value);
    map->size++;
}

const char *hashmap_get(HashMap *map, const char *key) {
    for (int i = 0; i < map->size; i++) {
        if (strcmp(map->entries[i].key, key) == 0) {
            return map->entries[i].value;
        }
    }
    return NULL;
}

void hashmap_free(HashMap *map) {
    for (int i = 0; i < map->size; i++) {
        free(map->entries[i].key);
        free(map->entries[i].value);
    }
    free(map->entries);
    free(map);
}
`)

var cppSource = []byte(`#include <string>
#include <vector>
#include <memory>

namespace db {

class Connection {
public:
    Connection(const std::string& host, int port);
    ~Connection();
    bool connect();
    void disconnect();

private:
    std::string host_;
    int port_;
    bool connected_ = false;
};

template<typename T>
class Repository {
public:
    virtual ~Repository() = default;
    virtual std::unique_ptr<T> findById(const std::string& id) = 0;
    virtual std::vector<T> findAll() = 0;
    virtual void save(const T& entity) = 0;
};

class UserRepository : public Repository<User> {
public:
    explicit UserRepository(std::shared_ptr<Connection> conn) : conn_(conn) {}
    std::unique_ptr<User> findById(const std::string& id) override;
    std::vector<User> findAll() override;
    void save(const User& entity) override;

private:
    std::shared_ptr<Connection> conn_;
};

} // namespace db
`)

var csharpSource = []byte(`using System;
using System.Collections.Generic;
using System.Threading.Tasks;

namespace MyApp.Services
{
    public interface IUserService
    {
        Task<User> GetByIdAsync(string id);
        Task<IEnumerable<User>> GetAllAsync();
        Task CreateAsync(User user);
    }

    public class UserService : IUserService
    {
        private readonly IRepository<User> _repo;
        private readonly ILogger _logger;

        public UserService(IRepository<User> repo, ILogger logger)
        {
            _repo = repo;
            _logger = logger;
        }

        public async Task<User> GetByIdAsync(string id)
        {
            _logger.Info($"Fetching user {id}");
            return await _repo.FindByIdAsync(id);
        }

        public async Task<IEnumerable<User>> GetAllAsync()
        {
            return await _repo.FindAllAsync();
        }

        public async Task CreateAsync(User user)
        {
            await _repo.SaveAsync(user);
        }
    }
}
`)

var jsSource = []byte(`const express = require('express');

class EventEmitter {
  constructor() {
    this.listeners = {};
  }

  on(event, callback) {
    if (!this.listeners[event]) {
      this.listeners[event] = [];
    }
    this.listeners[event].push(callback);
  }

  emit(event, ...args) {
    const handlers = this.listeners[event] || [];
    handlers.forEach(handler => handler(...args));
  }

  off(event, callback) {
    this.listeners[event] = (this.listeners[event] || [])
      .filter(h => h !== callback);
  }
}

function createServer(port) {
  const app = express();
  app.get('/health', (req, res) => res.json({ status: 'ok' }));
  return app.listen(port);
}

module.exports = { EventEmitter, createServer };
`)

var bashSource = []byte(`#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly LOG_FILE="/var/log/deploy.log"

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG_FILE"
}

check_dependencies() {
    local deps=("docker" "kubectl" "helm")
    for dep in "${deps[@]}"; do
        if ! command -v "$dep" &>/dev/null; then
            log "ERROR: $dep not found"
            exit 1
        fi
    done
}

deploy() {
    local env="$1"
    local version="$2"
    log "Deploying version $version to $env"
    docker build -t "myapp:$version" .
    docker push "myapp:$version"
    kubectl set image "deployment/myapp" "myapp=myapp:$version"
}

rollback() {
    local env="$1"
    log "Rolling back $env"
    kubectl rollout undo "deployment/myapp"
}

main() {
    check_dependencies
    deploy "$@"
}

main "$@"
`)

var phpSource = []byte(`<?php

namespace App\Services;

use App\Models\User;
use App\Repositories\UserRepository;

interface AuthServiceInterface
{
    public function authenticate(string $email, string $password): ?User;
    public function register(array $data): User;
}

class AuthService implements AuthServiceInterface
{
    private UserRepository $repo;

    public function __construct(UserRepository $repo)
    {
        $this->repo = $repo;
    }

    public function authenticate(string $email, string $password): ?User
    {
        $user = $this->repo->findByEmail($email);
        if ($user && password_verify($password, $user->password)) {
            return $user;
        }
        return null;
    }

    public function register(array $data): User
    {
        $data['password'] = password_hash($data['password'], PASSWORD_BCRYPT);
        return $this->repo->create($data);
    }
}
`)

var kotlinSource = []byte(`package com.example.service

import kotlinx.coroutines.flow.Flow

interface UserRepository {
    suspend fun findById(id: String): User?
    fun findAll(): Flow<User>
    suspend fun save(user: User)
    suspend fun delete(id: String)
}

data class User(val id: String, val name: String, val email: String)

class UserService(private val repo: UserRepository) {
    suspend fun getUser(id: String): User {
        return repo.findById(id) ?: throw NotFoundException("User $id not found")
    }

    suspend fun createUser(name: String, email: String): User {
        val user = User(generateId(), name, email)
        repo.save(user)
        return user
    }

    suspend fun deleteUser(id: String) {
        repo.delete(id)
    }

    private fun generateId(): String = java.util.UUID.randomUUID().toString()
}
`)

var swiftSource = []byte(`import Foundation

protocol Repository {
    associatedtype Entity
    func findById(_ id: String) async throws -> Entity?
    func findAll() async throws -> [Entity]
    func save(_ entity: Entity) async throws
}

struct User: Codable {
    let id: String
    var name: String
    var email: String
}

class UserService {
    private let repo: any Repository<User>

    init(repo: any Repository<User>) {
        self.repo = repo
    }

    func getUser(id: String) async throws -> User {
        guard let user = try await repo.findById(id) else {
            throw ServiceError.notFound
        }
        return user
    }

    func createUser(name: String, email: String) async throws -> User {
        let user = User(id: UUID().uuidString, name: name, email: email)
        try await repo.save(user)
        return user
    }
}

enum ServiceError: Error {
    case notFound
    case unauthorized
}
`)

var scalaSource = []byte(`package com.example

import scala.concurrent.Future
import scala.concurrent.ExecutionContext.Implicits.global

trait Repository[T] {
  def findById(id: String): Future[Option[T]]
  def findAll(): Future[Seq[T]]
  def save(entity: T): Future[Unit]
}

case class User(id: String, name: String, email: String)

class UserService(repo: Repository[User]) {
  def getUser(id: String): Future[User] = {
    repo.findById(id).map {
      case Some(user) => user
      case None => throw new NoSuchElementException(s"User $id not found")
    }
  }

  def createUser(name: String, email: String): Future[User] = {
    val user = User(java.util.UUID.randomUUID().toString, name, email)
    repo.save(user).map(_ => user)
  }
}

object UserService {
  def apply(repo: Repository[User]): UserService = new UserService(repo)
}
`)

var dartSource = []byte(`import 'dart:async';

abstract class Repository<T> {
  Future<T?> findById(String id);
  Future<List<T>> findAll();
  Future<void> save(T entity);
}

class User {
  final String id;
  String name;
  String email;

  User({required this.id, required this.name, required this.email});

  Map<String, dynamic> toJson() => {
    'id': id,
    'name': name,
    'email': email,
  };
}

class UserService {
  final Repository<User> _repo;

  UserService(this._repo);

  Future<User> getUser(String id) async {
    final user = await _repo.findById(id);
    if (user == null) throw Exception('User not found');
    return user;
  }

  Future<User> createUser(String name, String email) async {
    final user = User(id: DateTime.now().toString(), name: name, email: email);
    await _repo.save(user);
    return user;
  }
}
`)

var luaSource = []byte(`local M = {}

local Cache = {}
Cache.__index = Cache

function Cache.new(capacity)
    local self = setmetatable({}, Cache)
    self.data = {}
    self.capacity = capacity or 100
    self.size = 0
    return self
end

function Cache:get(key)
    return self.data[key]
end

function Cache:set(key, value)
    if self.size >= self.capacity then
        return false
    end
    if not self.data[key] then
        self.size = self.size + 1
    end
    self.data[key] = value
    return true
end

function Cache:delete(key)
    if self.data[key] then
        self.data[key] = nil
        self.size = self.size - 1
    end
end

function M.create_cache(capacity)
    return Cache.new(capacity)
end

return M
`)

var haskellSource = []byte(`module UserService where

import Data.Map (Map)
import qualified Data.Map as Map

data User = User
  { userId    :: String
  , userName  :: String
  , userEmail :: String
  } deriving (Show, Eq)

class Repository m where
  findById :: String -> m (Maybe User)
  findAll  :: m [User]
  save     :: User -> m ()

newtype UserService m = UserService { repo :: Repository m }

getUser :: Repository m => String -> m (Maybe User)
getUser = findById

createUser :: Repository m => String -> String -> m User
createUser name email = do
  let user = User { userId = "generated", userName = name, userEmail = email }
  save user
  return user
`)

var erlangSource = []byte(`-module(user_service).
-export([start/0, get_user/1, create_user/2, delete_user/1]).

-record(user, {id, name, email}).

start() ->
    ets:new(users, [named_table, set, public, {keypos, #user.id}]),
    ok.

get_user(Id) ->
    case ets:lookup(users, Id) of
        [User] -> {ok, User};
        [] -> {error, not_found}
    end.

create_user(Name, Email) ->
    Id = erlang:unique_integer([positive]),
    User = #user{id = Id, name = Name, email = Email},
    ets:insert(users, User),
    {ok, User}.

delete_user(Id) ->
    ets:delete(users, Id),
    ok.
`)

var clojureSource = []byte(`(ns myapp.user-service
  (:require [clojure.string :as str]))

(defprotocol Repository
  (find-by-id [this id])
  (find-all [this])
  (save! [this entity]))

(defrecord User [id name email])

(defn create-user [repo name email]
  (let [user (->User (str (java.util.UUID/randomUUID)) name email)]
    (save! repo user)
    user))

(defn get-user [repo id]
  (or (find-by-id repo id)
      (throw (ex-info "User not found" {:id id}))))

(defn validate-email [email]
  (re-matches #"[\w.+-]+@[\w-]+\.[\w.]+" email))
`)

var ocamlSource = []byte(`type user = {
  id : string;
  name : string;
  email : string;
}

module type REPOSITORY = sig
  val find_by_id : string -> user option
  val find_all : unit -> user list
  val save : user -> unit
end

module UserService (Repo : REPOSITORY) = struct
  let get_user id =
    match Repo.find_by_id id with
    | Some user -> user
    | None -> failwith "User not found"

  let create_user name email =
    let user = { id = "generated"; name; email } in
    Repo.save user;
    user

  let list_users () = Repo.find_all ()
end
`)

var zigSource = []byte(`const std = @import("std");

pub const User = struct {
    id: []const u8,
    name: []const u8,
    email: []const u8,
};

pub const Cache = struct {
    allocator: std.mem.Allocator,
    data: std.StringHashMap(User),

    pub fn init(allocator: std.mem.Allocator) Cache {
        return .{
            .allocator = allocator,
            .data = std.StringHashMap(User).init(allocator),
        };
    }

    pub fn get(self: *Cache, key: []const u8) ?User {
        return self.data.get(key);
    }

    pub fn put(self: *Cache, key: []const u8, value: User) !void {
        try self.data.put(key, value);
    }

    pub fn deinit(self: *Cache) void {
        self.data.deinit();
    }
};
`)

var rSource = []byte(`library(dplyr)

UserService <- R6::R6Class("UserService",
  private = list(
    data = NULL
  ),
  public = list(
    initialize = function() {
      private$data <- data.frame(
        id = character(),
        name = character(),
        email = character(),
        stringsAsFactors = FALSE
      )
    },
    get_user = function(id) {
      result <- private$data %>% filter(id == !!id)
      if (nrow(result) == 0) stop("User not found")
      result
    },
    create_user = function(name, email) {
      new_user <- data.frame(id = uuid::UUIDgenerate(), name = name, email = email)
      private$data <- rbind(private$data, new_user)
      new_user
    },
    list_users = function() {
      private$data
    }
  )
)
`)

var cssSource = []byte(`:root {
  --primary: #3b82f6;
  --secondary: #64748b;
  --bg: #0f172a;
}

.container {
  max-width: 1200px;
  margin: 0 auto;
  padding: 0 1rem;
}

.card {
  background: var(--bg);
  border-radius: 8px;
  padding: 1.5rem;
  box-shadow: 0 2px 4px rgba(0, 0, 0, 0.1);
}

.card__title {
  font-size: 1.25rem;
  color: var(--primary);
  margin-bottom: 0.5rem;
}

@media (max-width: 768px) {
  .container { padding: 0 0.5rem; }
  .card { padding: 1rem; }
}
`)

var htmlSource = []byte(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Dashboard</title>
  <link rel="stylesheet" href="/styles.css">
</head>
<body>
  <header class="header">
    <nav class="nav">
      <a href="/" class="nav__logo">App</a>
      <ul class="nav__links">
        <li><a href="/dashboard">Dashboard</a></li>
        <li><a href="/settings">Settings</a></li>
      </ul>
    </nav>
  </header>
  <main class="main">
    <section class="dashboard">
      <h1>Welcome</h1>
      <div id="chart-container"></div>
    </section>
  </main>
  <script src="/app.js"></script>
</body>
</html>
`)

var markdownSource = []byte(`# API Reference

## Authentication

### POST /auth/login

Authenticates a user and returns a JWT token.

**Request Body:**

| Field    | Type   | Required |
|----------|--------|----------|
| email    | string | yes      |
| password | string | yes      |

**Response:**

` + "```json" + `
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "expires_in": 3600
}
` + "```" + `

## Users

### GET /users/:id

Returns a user by ID.

### POST /users

Creates a new user.
`)

var yamlSource = []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
  labels:
    app: myapp
    version: v1
spec:
  replicas: 3
  selector:
    matchLabels:
      app: myapp
  template:
    metadata:
      labels:
        app: myapp
    spec:
      containers:
        - name: myapp
          image: myapp:latest
          ports:
            - containerPort: 8080
          env:
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: db-secret
                  key: url
          resources:
            limits:
              memory: "256Mi"
              cpu: "500m"
`)

var tomlSource = []byte(`[package]
name = "myapp"
version = "0.1.0"
edition = "2021"
authors = ["Dev <dev@example.com>"]

[dependencies]
serde = { version = "1.0", features = ["derive"] }
tokio = { version = "1", features = ["full"] }
axum = "0.7"

[dev-dependencies]
criterion = "0.5"

[[bin]]
name = "server"
path = "src/main.rs"

[profile.release]
opt-level = 3
lto = true
`)

var sqlSource = []byte(`CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    email VARCHAR(255) UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_users_email ON users(email);

CREATE TABLE orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id),
    total DECIMAL(10, 2) NOT NULL,
    status VARCHAR(50) DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION get_user_orders(p_user_id UUID)
RETURNS TABLE(order_id UUID, total DECIMAL, status VARCHAR) AS $$
BEGIN
    RETURN QUERY
    SELECT id, total, status FROM orders WHERE user_id = p_user_id;
END;
$$ LANGUAGE plpgsql;
`)

var protobufSource = []byte(`syntax = "proto3";

package myapp.v1;

option go_package = "myapp/proto/v1";

message User {
  string id = 1;
  string name = 2;
  string email = 3;
  int64 created_at = 4;
}

message GetUserRequest {
  string id = 1;
}

message CreateUserRequest {
  string name = 1;
  string email = 2;
}

service UserService {
  rpc GetUser(GetUserRequest) returns (User);
  rpc CreateUser(CreateUserRequest) returns (User);
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse);
}

message ListUsersRequest {
  int32 page_size = 1;
  string page_token = 2;
}

message ListUsersResponse {
  repeated User users = 1;
  string next_page_token = 2;
}
`)

var dockerfileSource = []byte(`FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /server ./cmd/server/

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /server .
EXPOSE 8080
ENTRYPOINT ["./server"]
`)

var hclSource = []byte(`terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  type    = string
  default = "us-east-1"
}

resource "aws_instance" "web" {
  ami           = "ami-0c55b159cbfafe1f0"
  instance_type = "t3.micro"

  tags = {
    Name = "web-server"
  }
}

output "instance_ip" {
  value = aws_instance.web.public_ip
}
`)

// =============================================================================
// Benchmark functions for all languages
// =============================================================================

func BenchmarkCExtractor(b *testing.B) {
	e := NewCExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("hashmap.c", cSource)
	}
}

func BenchmarkCppExtractor(b *testing.B) {
	e := NewCppExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("repository.cpp", cppSource)
	}
}

func BenchmarkCSharpExtractor(b *testing.B) {
	e := NewCSharpExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("UserService.cs", csharpSource)
	}
}

func BenchmarkJavaScriptExtractor(b *testing.B) {
	e := NewJavaScriptExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("emitter.js", jsSource)
	}
}

func BenchmarkBashExtractor(b *testing.B) {
	e := NewBashExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("deploy.sh", bashSource)
	}
}

func BenchmarkPHPExtractor(b *testing.B) {
	e := NewPHPExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("AuthService.php", phpSource)
	}
}

func BenchmarkKotlinExtractor(b *testing.B) {
	e := NewKotlinExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("UserService.kt", kotlinSource)
	}
}

func BenchmarkSwiftExtractor(b *testing.B) {
	e := NewSwiftExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("UserService.swift", swiftSource)
	}
}

func BenchmarkScalaExtractor(b *testing.B) {
	e := NewScalaExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("UserService.scala", scalaSource)
	}
}

func BenchmarkDartExtractor(b *testing.B) {
	e := NewDartExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("user_service.dart", dartSource)
	}
}

func BenchmarkLuaExtractor(b *testing.B) {
	e := NewLuaExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("cache.lua", luaSource)
	}
}

func BenchmarkHaskellExtractor(b *testing.B) {
	e := NewHaskellExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("UserService.hs", haskellSource)
	}
}

func BenchmarkErlangExtractor(b *testing.B) {
	e := NewErlangExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("user_service.erl", erlangSource)
	}
}

func BenchmarkClojureExtractor(b *testing.B) {
	e := NewClojureExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("user_service.clj", clojureSource)
	}
}

func BenchmarkOCamlExtractor(b *testing.B) {
	e := NewOCamlExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("user_service.ml", ocamlSource)
	}
}

func BenchmarkZigExtractor(b *testing.B) {
	e := NewZigExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("cache.zig", zigSource)
	}
}

func BenchmarkRExtractor(b *testing.B) {
	e := NewRExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("user_service.R", rSource)
	}
}

func BenchmarkCSSExtractor(b *testing.B) {
	e := NewCSSExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("styles.css", cssSource)
	}
}

func BenchmarkHTMLExtractor(b *testing.B) {
	e := NewHTMLExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("index.html", htmlSource)
	}
}

func BenchmarkMarkdownExtractor(b *testing.B) {
	e := NewMarkdownExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("api.md", markdownSource)
	}
}

func BenchmarkYAMLExtractor(b *testing.B) {
	e := NewYAMLExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("deployment.yaml", yamlSource)
	}
}

func BenchmarkTOMLExtractor(b *testing.B) {
	e := NewTOMLExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("Cargo.toml", tomlSource)
	}
}

func BenchmarkSQLExtractor(b *testing.B) {
	e := NewSQLExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("schema.sql", sqlSource)
	}
}

func BenchmarkProtobufExtractor(b *testing.B) {
	e := NewProtobufExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("user.proto", protobufSource)
	}
}

func BenchmarkDockerfileExtractor(b *testing.B) {
	e := NewDockerfileExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("Dockerfile", dockerfileSource)
	}
}

func BenchmarkHCLExtractor(b *testing.B) {
	e := NewHCLExtractor()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Extract("main.tf", hclSource)
	}
}
