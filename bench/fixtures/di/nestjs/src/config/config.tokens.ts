// String-keyed injection tokens. These are the NestJS idiom for
// providing values that have no class identity — environment URLs,
// configuration primitives, feature flags. A consumer asks for them
// via `@Inject(DATABASE_URL)`; the Module provides them via
// `{ provide: DATABASE_URL, useValue: '...' }`. No type-aware
// resolution can cross this boundary.
export const DATABASE_URL = 'DATABASE_URL';
export const FEATURE_FLAGS = 'FEATURE_FLAGS';
