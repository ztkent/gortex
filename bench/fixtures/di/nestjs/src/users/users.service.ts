import { Injectable, NotFoundException } from '@nestjs/common';
import { User } from './user.entity';

@Injectable()
export class UsersService {
  private readonly users: Map<string, User> = new Map();

  async findOne(id: string): Promise<User> {
    const u = this.users.get(id);
    if (!u) {
      throw new NotFoundException(`user ${id} not found`);
    }
    return u;
  }

  async findAll(): Promise<User[]> {
    return Array.from(this.users.values());
  }

  async create(email: string, name: string): Promise<User> {
    const id = `usr_${this.users.size + 1}`;
    const user = new User(id, email, name);
    this.users.set(id, user);
    return user;
  }
}
