import 'dotenv/config';
import { Template, defaultBuildLogger } from 'e2b';
import { template } from './template';

async function main() {
  const buildInfo = await Template.build(template, 'github-runner-ubuntu-24-04', {
    cpuCount: 8,
    memoryMB: 8192,
    apiKey: process.env.E2B_API_KEY,
    domain: process.env.E2B_DOMAIN,
    onBuildLogs: defaultBuildLogger(),
  });

  console.log(buildInfo);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
