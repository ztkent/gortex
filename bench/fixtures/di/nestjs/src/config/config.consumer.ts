import { Injectable } from '@nestjs/common';
import { ConfigService } from './config.service';

// Second-hop consumer: ConfigConsumer calls ConfigService, which itself
// received its values via @Inject(TOKEN). The test is whether the graph
// can reach *from* ConfigConsumer *to* the methods on ConfigService —
// standard typed injection, should work — and whether the TOKEN-keyed
// providers show up in any cross-reference (they won't, without DI
// extraction).
@Injectable()
export class ConfigConsumer {
  constructor(private readonly config: ConfigService) {}

  describe(): string {
    return `db=${this.config.getDatabaseUrl()} flags=${JSON.stringify(
      this.config.isEnabled('beta'),
    )}`;
  }
}
