import { Module } from '@nestjs/common';
import { UsersModule } from './users/users.module';
import { AuthModule } from './auth/auth.module';
import { NotificationsModule } from './notifications/notifications.module';
import { ConfigModule } from './config/config.module';
import { BillingModule } from './billing/billing.module';
import { FeatureModule } from './feature/feature.module';

@Module({
  imports: [
    UsersModule,
    AuthModule,
    NotificationsModule,
    ConfigModule,
    BillingModule,
    FeatureModule,
  ],
})
export class AppModule {}
