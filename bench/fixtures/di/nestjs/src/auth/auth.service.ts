import { Injectable, UnauthorizedException } from '@nestjs/common';
import { UsersService } from '../users/users.service';

@Injectable()
export class AuthService {
  constructor(private readonly usersService: UsersService) {}

  async validate(userId: string, token: string): Promise<boolean> {
    const user = await this.usersService.findOne(userId);
    if (!user || token !== `tok_${user.id}`) {
      throw new UnauthorizedException('invalid token');
    }
    return true;
  }

  async issueToken(userId: string): Promise<string> {
    const user = await this.usersService.findOne(userId);
    return `tok_${user.id}`;
  }
}
