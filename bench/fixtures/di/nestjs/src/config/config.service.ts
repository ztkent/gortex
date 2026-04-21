import { Inject, Injectable } from '@nestjs/common';
import { DATABASE_URL, FEATURE_FLAGS } from './config.tokens';

@Injectable()
export class ConfigService {
  // Both constructor params receive their values via string-keyed
  // @Inject — no type info survives into the graph for the binder to
  // traverse.
  constructor(
    @Inject(DATABASE_URL) private readonly dbUrl: string,
    @Inject(FEATURE_FLAGS) private readonly flags: Record<string, boolean>,
  ) {}

  getDatabaseUrl(): string {
    return this.dbUrl;
  }

  isEnabled(flag: string): boolean {
    return Boolean(this.flags[flag]);
  }
}
