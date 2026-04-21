// Separate file for the injection token so the module import graph
// reflects real NestJS practice — consumers import the token without
// pulling in the module and its factory provider.
export const DB_CONNECTION = 'DB_CONNECTION';
