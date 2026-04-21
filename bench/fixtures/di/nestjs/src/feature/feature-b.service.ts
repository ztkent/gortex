import { forwardRef, Inject, Injectable } from '@nestjs/common';
import { FeatureAService } from './feature-a.service';

@Injectable()
export class FeatureBService {
  constructor(
    @Inject(forwardRef(() => FeatureAService))
    private readonly a: FeatureAService,
  ) {}

  doB(): string {
    return 'B';
  }

  callA(): string {
    return this.a.doA();
  }
}
