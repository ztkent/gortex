import {
  CanActivate,
  ExecutionContext,
  Injectable,
  UnauthorizedException,
} from '@nestjs/common';
import { AuthService } from './auth.service';

@Injectable()
export class AuthGuard implements CanActivate {
  constructor(private readonly authService: AuthService) {}

  async canActivate(context: ExecutionContext): Promise<boolean> {
    const req = context.switchToHttp().getRequest();
    const userId = req.headers['x-user-id'];
    const token = req.headers['x-token'];
    if (!userId || !token) {
      throw new UnauthorizedException('missing credentials');
    }
    return this.authService.validate(userId, token);
  }
}
