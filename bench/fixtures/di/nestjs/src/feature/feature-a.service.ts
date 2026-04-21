import { forwardRef, Inject, Injectable } from '@nestjs/common';
import { FeatureBService } from './feature-b.service';

// Two services that reference each other. NestJS requires forwardRef()
// to break the import cycle; the lazy arrow function `() => FeatureBService`
// is evaluated after module init. Static extraction can't call the arrow
// at parse time, so the `b` parameter's declared type is effectively
// unknown (or `any`) at extraction. Type-aware resolution can't link
// `this.b.doB()` to FeatureBService.doB without following the forwardRef.
@Injectable()
export class FeatureAService {
  constructor(
    @Inject(forwardRef(() => FeatureBService))
    private readonly b: FeatureBService,
  ) {}

  doA(): string {
    return this.b.doB() + ' from A';
  }
}
