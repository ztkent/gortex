import { Module } from '@nestjs/common';
import { FeatureAService } from './feature-a.service';
import { FeatureBService } from './feature-b.service';

@Module({
  providers: [FeatureAService, FeatureBService],
  exports: [FeatureAService, FeatureBService],
})
export class FeatureModule {}
