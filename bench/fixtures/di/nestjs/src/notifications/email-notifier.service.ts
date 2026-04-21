import { Injectable } from '@nestjs/common';
import { Notifier } from './notifier.interface';

@Injectable()
export class EmailNotifier extends Notifier {
  async notify(userId: string, message: string): Promise<void> {
    // In real code this would call an SMTP client; here the logic is
    // deliberately trivial — the fixture only needs the method to exist.
    console.log(`email to ${userId}: ${message}`);
  }
}
