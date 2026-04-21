import { Module } from '@nestjs/common';
import { ConfigModule } from '../config/config.module';
import { ConfigService } from '../config/config.service';
import { DB_CONNECTION } from './billing.tokens';
import { DatabaseConnection } from './database.connection';
import { BillingService } from './billing.service';

// Factory provider: the binding DB_CONNECTION → DatabaseConnection is
// produced by calling `dbFactory(cfg)` at module bootstrap. `inject:
// [ConfigService]` is how Nest passes the factory its dependencies.
// There is no `new DatabaseConnection(...)` call anywhere else in the
// codebase; any graph analysis that wants to traverse "consumers of
// DB_CONNECTION → DatabaseConnection methods" must read this factory.
const dbFactory = (cfg: ConfigService) =>
  new DatabaseConnection(cfg.getDatabaseUrl());

@Module({
  imports: [ConfigModule],
  providers: [
    {
      provide: DB_CONNECTION,
      useFactory: dbFactory,
      inject: [ConfigService],
    },
    BillingService,
  ],
  exports: [BillingService],
})
export class BillingModule {}
