import { Body, Controller, Post, UseGuards } from '@nestjs/common';
import { Notifier } from './notifier.interface';
import { AuthGuard } from '../auth/auth.guard';

@Controller('notifications')
export class NotificationsController {
  // Injected by the abstract base class — the runtime instance will
  // be EmailNotifier (see notifications.module.ts), but the static
  // type here is `Notifier` so a type-aware resolver has no way to
  // pick the right concrete method.
  constructor(private readonly notifier: Notifier) {}

  // @UseGuards binds the guard's canActivate() to this route. There is
  // no explicit call site to AuthGuard.canActivate anywhere in source —
  // the framework invokes it at request time. Without decorator-dispatch
  // extraction, get_callers on canActivate is empty for this endpoint.
  @Post('send')
  @UseGuards(AuthGuard)
  async send(@Body('userId') userId: string, @Body('message') message: string): Promise<void> {
    await this.notifier.notify(userId, message);
  }
}
