//go:build llama

// Package llm is a thin CGO wrapper around the llama.cpp C API.
// Build with `-tags llama`; requires libllama at link time
// (Homebrew: `brew install llama.cpp`, pkg-config provides flags).
package llm

/*
#cgo pkg-config: llama ggml
#include <llama.h>
#include <ggml-backend.h>
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

var (
	backendOnce sync.Once
)

func initBackend() {
	backendOnce.Do(func() {
		// ggml backends (CPU, Metal, CUDA, ...) ship as plugins in
		// recent ggml; llama_backend_init no longer registers them.
		C.ggml_backend_load_all()
		C.llama_backend_init()
	})
}

type Model struct {
	m     *C.struct_llama_model
	vocab *C.struct_llama_vocab
}

type Context struct {
	model *Model
	ctx   *C.struct_llama_context
	smpl  *C.struct_llama_sampler
	nCtx  int
}

func LoadModel(path string, gpuLayers int) (*Model, error) {
	initBackend()
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	params := C.llama_model_default_params()
	params.n_gpu_layers = C.int32_t(gpuLayers)

	m := C.llama_model_load_from_file(cPath, params)
	if m == nil {
		return nil, fmt.Errorf("llama: failed to load model from %s", path)
	}
	v := C.llama_model_get_vocab(m)
	if v == nil {
		C.llama_model_free(m)
		return nil, errors.New("llama: model has no vocab")
	}
	return &Model{m: m, vocab: v}, nil
}

func (m *Model) Close() {
	if m == nil || m.m == nil {
		return
	}
	C.llama_model_free(m.m)
	m.m = nil
	m.vocab = nil
}

func (m *Model) NewContext(nCtx, nThreads int) (*Context, error) {
	if m == nil || m.m == nil {
		return nil, errors.New("llama: model is closed")
	}
	cp := C.llama_context_default_params()
	cp.n_ctx = C.uint32_t(nCtx)
	cp.n_batch = C.uint32_t(nCtx)
	cp.no_perf = C.bool(true)

	ctx := C.llama_init_from_model(m.m, cp)
	if ctx == nil {
		return nil, errors.New("llama: failed to create context")
	}
	if nThreads > 0 {
		C.llama_set_n_threads(ctx, C.int32_t(nThreads), C.int32_t(nThreads))
	}

	c := &Context{model: m, ctx: ctx, nCtx: nCtx}
	if err := c.rebuildSampler(""); err != nil {
		C.llama_free(ctx)
		return nil, err
	}
	return c, nil
}

// rebuildSampler frees the current chain (if any) and constructs a new
// one. If gbnf is non-empty, a grammar sampler is inserted before the
// greedy sampler so the model can only emit tokens that keep the
// grammar accepting.
func (c *Context) rebuildSampler(gbnf string) error {
	if c.smpl != nil {
		C.llama_sampler_free(c.smpl)
		c.smpl = nil
	}
	sp := C.llama_sampler_chain_default_params()
	sp.no_perf = C.bool(true)
	chain := C.llama_sampler_chain_init(sp)
	if chain == nil {
		return errors.New("llama: failed to create sampler chain")
	}
	if gbnf != "" {
		cG := C.CString(gbnf)
		cR := C.CString("root")
		gs := C.llama_sampler_init_grammar(c.model.vocab, cG, cR)
		C.free(unsafe.Pointer(cG))
		C.free(unsafe.Pointer(cR))
		if gs == nil {
			C.llama_sampler_free(chain)
			return errors.New("llama: failed to init grammar sampler (bad GBNF?)")
		}
		C.llama_sampler_chain_add(chain, gs)
	}
	C.llama_sampler_chain_add(chain, C.llama_sampler_init_greedy())
	c.smpl = chain
	return nil
}

// SetGrammar replaces the sampler chain with one that enforces the
// given GBNF grammar. Passing "" removes any grammar constraint.
func (c *Context) SetGrammar(gbnf string) error {
	return c.rebuildSampler(gbnf)
}

// Reset clears the KV cache and resets the sampler chain so the next
// Generate call starts fresh. Cheap; does not reallocate buffers.
// Required between turns when a grammar sampler is in use, since the
// grammar is stateful and will reject everything after one match.
func (c *Context) Reset() {
	mem := C.llama_get_memory(c.ctx)
	C.llama_memory_clear(mem, C.bool(false))
	if c.smpl != nil {
		C.llama_sampler_reset(c.smpl)
	}
}

func (c *Context) Close() {
	if c == nil {
		return
	}
	if c.smpl != nil {
		C.llama_sampler_free(c.smpl)
		c.smpl = nil
	}
	if c.ctx != nil {
		C.llama_free(c.ctx)
		c.ctx = nil
	}
}

func (c *Context) tokenize(text string, addBOS, parseSpecial bool) ([]C.llama_token, error) {
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))
	cLen := C.int32_t(len(text))

	// llama_tokenize returns -n_required when buf is too small; probe first.
	need := -C.llama_tokenize(c.model.vocab, cText, cLen, nil, 0,
		C.bool(addBOS), C.bool(parseSpecial))
	if need <= 0 {
		return nil, fmt.Errorf("llama: tokenize probe returned %d", need)
	}
	toks := make([]C.llama_token, need)
	n := C.llama_tokenize(c.model.vocab, cText, cLen,
		(*C.llama_token)(unsafe.Pointer(&toks[0])), need,
		C.bool(addBOS), C.bool(parseSpecial))
	if n < 0 {
		return nil, fmt.Errorf("llama: tokenize failed: %d", n)
	}
	return toks[:n], nil
}

func (c *Context) tokenToPiece(tok C.llama_token) string {
	buf := make([]byte, 64)
	n := C.llama_token_to_piece(c.model.vocab, tok,
		(*C.char)(unsafe.Pointer(&buf[0])), C.int32_t(len(buf)),
		0, C.bool(true))
	if n < 0 {
		buf = make([]byte, -n)
		n = C.llama_token_to_piece(c.model.vocab, tok,
			(*C.char)(unsafe.Pointer(&buf[0])), C.int32_t(len(buf)),
			0, C.bool(true))
		if n < 0 {
			return ""
		}
	}
	return string(buf[:n])
}

// Generate runs greedy decoding from prompt. emit is called once per
// generated piece (typically a sub-word). Returning false from emit stops
// generation early. Returns the total number of tokens generated.
func (c *Context) Generate(prompt string, maxTokens int, emit func(string) bool) (int, error) {
	toks, err := c.tokenize(prompt, true, true)
	if err != nil {
		return 0, err
	}
	if len(toks) == 0 {
		return 0, errors.New("llama: prompt tokenized to zero tokens")
	}
	if len(toks) >= c.nCtx {
		return 0, fmt.Errorf("llama: prompt (%d tokens) exceeds context (%d)", len(toks), c.nCtx)
	}

	// Prefill the prompt.
	batch := C.llama_batch_get_one(&toks[0], C.int32_t(len(toks)))
	if rc := C.llama_decode(c.ctx, batch); rc != 0 {
		return 0, fmt.Errorf("llama: prefill decode failed: %d", rc)
	}

	eos := C.llama_vocab_eos(c.model.vocab)
	generated := 0
	tok := C.llama_sampler_sample(c.smpl, c.ctx, -1)

	for generated < maxTokens {
		if tok == eos {
			break
		}
		piece := c.tokenToPiece(tok)
		if piece != "" {
			if !emit(piece) {
				return generated, nil
			}
		}
		generated++

		single := [1]C.llama_token{tok}
		b := C.llama_batch_get_one(&single[0], 1)
		if rc := C.llama_decode(c.ctx, b); rc != 0 {
			return generated, fmt.Errorf("llama: step decode failed at %d: %d", generated, rc)
		}
		tok = C.llama_sampler_sample(c.smpl, c.ctx, -1)
	}
	return generated, nil
}
