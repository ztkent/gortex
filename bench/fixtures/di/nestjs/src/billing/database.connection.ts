// Concrete class wired up through a factory provider. The only place
// the binding `"DB_CONNECTION" → DatabaseConnection` appears is inside
// a `useFactory` in billing.module.ts — no `new DatabaseConnection(...)`
// call survives anywhere else, so Tier-2 type resolution can't reach
// this class from consumers that inject `@Inject('DB_CONNECTION')`.
export class DatabaseConnection {
  constructor(public readonly url: string) {}

  async query<T>(sql: string): Promise<T[]> {
    console.log(`[${this.url}] ${sql}`);
    return [];
  }

  async close(): Promise<void> {
    // Close the pool in real code.
  }
}
