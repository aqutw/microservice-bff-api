import { fileURLToPath } from 'node:url';

const nextConfig = {
  output: 'standalone',
  turbopack: { root: fileURLToPath(new URL('.', import.meta.url)) }
};
export default nextConfig;
