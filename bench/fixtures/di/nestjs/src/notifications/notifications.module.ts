import { Module } from '@nestjs/common';
import { Notifier } from './notifier.interface';
import { EmailNotifier } from './email-notifier.service';
import { SmsNotifier } from './sms-notifier.service';
import { NotificationsController } from './notifications.controller';
import { AuthModule } from '../auth/auth.module';

// The `provide: Notifier, useClass: EmailNotifier` binding is the only
// place a static reader can learn that NotificationsController's
// `notifier` parameter resolves to EmailNotifier specifically.
@Module({
  imports: [AuthModule],
  controllers: [NotificationsController],
  providers: [
    { provide: Notifier, useClass: EmailNotifier },
    SmsNotifier,
  ],
  exports: [Notifier, SmsNotifier],
})
export class NotificationsModule {}
