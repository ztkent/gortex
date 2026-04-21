import { Injectable } from '@nestjs/common';
import { Notifier } from './notifier.interface';

// Second implementation of the same abstract class. Makes the
// abstract-injection case ambiguous — module-level binding is what
// actually picks between EmailNotifier and SmsNotifier. This is the
// shape Tier-2 type-aware resolution cannot handle.
@Injectable()
export class SmsNotifier extends Notifier {
  async notify(userId: string, message: string): Promise<void> {
    console.log(`sms to ${userId}: ${message}`);
  }
}
