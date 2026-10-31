### Security

- **The default oj dev server no longer serves files outside the public dir
  and the allowed roots through an asset query in a server-side render.**
  Before oj 0.2.50, `/@ssr-module?id=<root>/.env?raw` returned the file and a
  root-relative id that climbed out of the public dir
  (`/../../.ssh/id_rsa?raw`) was served, past `server.fs.deny`.
