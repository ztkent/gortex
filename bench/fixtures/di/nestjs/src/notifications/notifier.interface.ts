// Abstract base class used as the injection token — NestJS supports
// `constructor(private n: Notifier)` where Notifier is an abstract class
// and the Module provides `{ provide: Notifier, useClass: EmailNotifier }`.
// Type-aware resolution can't cross this indirection without knowing the
// provider binding; exactly the gap option 1's DI extractor targets.
export abstract class Notifier {
  abstract notify(userId: string, message: string): Promise<void>;
}
