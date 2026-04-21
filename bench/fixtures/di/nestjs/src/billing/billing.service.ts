import { Inject, Injectable } from '@nestjs/common';
import { DatabaseConnection } from './database.connection';
import { DB_CONNECTION } from './billing.tokens';

// Consumer of the factory-provided DatabaseConnection. The `db` param's
// declared type IS DatabaseConnection — so with parameter-property typing
// the receiver type resolution should work for `this.db.query(...)`. But
// the *binding* from the token DB_CONNECTION to DatabaseConnection lives
// only inside the factory — a graph walk from any consumer to "who
// produces DB_CONNECTION" has no edge to follow without DI extraction.
@Injectable()
export class BillingService {
  constructor(
    @Inject(DB_CONNECTION) private readonly db: DatabaseConnection,
  ) {}

  async charge(userId: string, cents: number): Promise<void> {
    await this.db.query(`INSERT INTO charges (user_id, amount) VALUES ('${userId}', ${cents})`);
  }

  async totals(userId: string): Promise<number> {
    const rows = await this.db.query<{ total: number }>(
      `SELECT SUM(amount) AS total FROM charges WHERE user_id = '${userId}'`,
    );
    return rows[0]?.total ?? 0;
  }
}
