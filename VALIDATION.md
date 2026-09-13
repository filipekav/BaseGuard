# Validação da implementação

Executado em 13/09/2026 no ambiente Windows/AMD64 de desenvolvimento:

- `go test ./... -count=1 -cover`: aprovado. Cobertura do pacote `internal/app`: 60,4%.
- `go vet ./...`: aprovado.
- Compilação nativa Windows e compilação cruzada Linux/ARM64 e Linux/AMD64 com CGO desativado.
- `docker compose config -q` nos três manifests, usando Docker Compose oficial 5.5.1: aprovado.
- Conferência no Docker Hub: todas as imagens usadas estão disponíveis para ARM64 e AMD64.
- Revisão em navegador: login, cadastro, frequência semanal, teste de conexão, disparo manual, atualização do histórico por HTMX, tela de celular e ausência de erros no console.

Os testes locais exercitam persistência após reabertura, cifragem de credenciais, chave incorreta/ausente, agendamentos, concorrência da fila, exclusividade da execução, reinício, preservação de backups após falhas, retenção, caminhos inválidos, falha de escrita, timeout de subprocesso, ocultação de senha em erros, login, CSRF, cadastro/edição/remoção e download autenticado.

## Validações que exigem outro ambiente

Não há Docker Engine ou servidores PostgreSQL/MySQL disponíveis no ambiente de desenvolvimento. Por isso, **a imagem Docker não foi construída aqui e os testes de dump/restauração reais não foram executados**. A suíte `TestIntegrationRestore`, o `compose.test.yaml` e o workflow de CI foram preparados para executar essa validação. A validação de configuração Compose não comprova a execução dos containers.

Também não foram medidos memória, CPU ou tempo de backup em hardware ARM64. A compilação cruzada confirma que o código compila para a arquitetura; não substitui a execução no servidor.

Antes de proteger os bancos reais:

1. Confirme as versões de PostgreSQL e MySQL. A imagem padrão traz clientes PostgreSQL 18 e MySQL 8.0; outras séries MySQL exigem adaptação.
2. No servidor, construa a imagem e execute a suíte de integração descrita no README.
3. Teste cada conexão e realize um backup manual.
4. Restaure em banco descartável, conferindo dados, views, rotinas, triggers, eventos, extensões e permissões aplicáveis.
5. Meça o consumo com um banco representativo e só então ative os agendamentos.
