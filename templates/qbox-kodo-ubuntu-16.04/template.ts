import { readFileSync } from 'node:fs';
import { Template } from 'e2b';

const dockerfile = readFileSync(new URL('./e2b.Dockerfile', import.meta.url), 'utf8');

export const template = Template({
  fileContextPath: '.',
  fileIgnorePatterns: ['node_modules', 'package-lock.json'],
}).fromDockerfile(dockerfile);
