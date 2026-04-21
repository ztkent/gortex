import { Module } from '@nestjs/common';
import { DATABASE_URL, FEATURE_FLAGS } from './config.tokens';
import { ConfigService } from './config.service';
import { ConfigConsumer } from './config.consumer';

@Module({
  providers: [
    { provide: DATABASE_URL, useValue: 'postgres://localhost/test' },
    { provide: FEATURE_FLAGS, useValue: { beta: true } },
    ConfigService,
    ConfigConsumer,
  ],
  exports: [ConfigService, ConfigConsumer],
})
export class ConfigModule {}
