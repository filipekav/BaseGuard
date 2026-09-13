# BaseGuard

Painel local para backups completos de **PostgreSQL e MySQL**, com interface em português, agendamento, fila persistente, retenção e download. Um processo Go, SQLite e clientes oficiais de dump, em um container ARM64 ou AMD64.

## O que está incluído

- Administrador único (`admin`), senha com bcrypt, sessões de 12 horas, proteção CSRF e limitação de tentativas de login.
- Cadastro e edição de conexão, teste de acesso/versão, TLS e CA personalizada.
- Agendamento diário, semanal ou a cada N horas. Padrão: 02h em `America/Sao_Paulo`, desativado até você ativar.
- Um backup por vez. Fila em SQLite e no máximo um trabalho pendente por banco.
- PostgreSQL `.dump` customizado com compressão; MySQL `.sql.gz` com compressão rápida.
- Retenção por banco (padrão: 7 arquivos), timeout (padrão: 120 minutos), histórico das últimas 200 execuções na tela e checksum SHA-256.
- Arquivo `.partial` durante a execução. O download só é liberado depois de sucesso, sincronização e renomeação do arquivo.
- Credenciais cifradas com AES-256-GCM; chave mestra em volume separado. Senhas dos bancos não vão nos argumentos de processos ou nos logs.

## Compatibilidade

A imagem padrão usa Ubuntu 24.04, **cliente PostgreSQL 18 e cliente MySQL 8.0**. O teste de conexão e cada backup verificam a compatibilidade antes do dump:

| Servidor | Comportamento da imagem padrão |
|---|---|
| PostgreSQL 14–18 | Aceito pelo verificador; use a suíte de restauração para homologar suas versões e extensões |
| PostgreSQL mais novo que o cliente | Recusado; atualize `PG_MAJOR` no build e valide a restauração |
| MySQL 8.0, tabelas InnoDB | Aceito pelo verificador |
| MySQL de outra série, como 8.4 | Recusado; exige imagem adaptada com cliente da mesma série e teste de recuperação |
| MySQL com tabelas fora do InnoDB | Recusado para evitar anunciar consistência que uma transação não garante |
| MariaDB | Fora do escopo desta versão |

**As versões dos seus servidores ainda precisam ser confirmadas.** A checagem de versão não substitui homologação de extensões, permissões e restauração. Evite alterações de estrutura (DDL) durante dumps MySQL. Tenha um usuário dedicado com permissões suficientes para tabelas, views, triggers, rotinas e eventos; o backup falha se faltar permissão.

## Instalação no Linux / CasaOS

### Instalar pelo painel do CasaOS

Após a publicação da imagem no GitHub Actions:

1. Abra **Instalação personalizada** e clique no ícone **Importar**, no canto superior direito.
2. Importe este arquivo (ou cole o conteúdo): **[compose.casaos.yaml](https://raw.githubusercontent.com/filipekav/BaseGuard/main/compose.casaos.yaml)**.
3. Confira a porta externa `8080` e os diretórios em `/DATA`. Se a porta estiver ocupada, altere a porta do host e a porta da Web UI, mantendo a porta interna `8080`.
4. Clique em **Instalar**. O CasaOS baixa `ghcr.io/filipekav/baseguard:latest`, escolhendo ARM64 ou AMD64 automaticamente.
5. Abra `http://IP-DO-SERVIDOR:8080`. O usuário é **admin** e a senha inicial aleatória aparece nos logs do container. Troque-a em **Configurações**.

O primeiro início prepara automaticamente os diretórios dedicados de dados, chave e backups locais. O processo principal executa como UID/GID **10001**; o entrypoint usa root apenas para preparar esses três diretórios e depois abandona os privilégios. Não há modo privilegiado nem acesso ao Docker socket.

Se preferir preencher o formulário manualmente, use imagem **`ghcr.io/filipekav/baseguard`**, tag **`latest`** e título **BaseGuard**. A importação do Compose é recomendada porque também configura volumes, permissões de inicialização, ambiente e healthcheck.

O destino automático `/DATA/Backups/baseguard` é para **disco local**. Para NAS/USB use a configuração manual abaixo e desative `BASEGUARD_INIT_LOCAL_DESTINATION`; nunca inicialize automaticamente um compartilhamento que pode estar desmontado. Em instalações existentes, um marcador ausente não é recriado automaticamente.

### Publicação da imagem

Cada push em `main` executa testes e restauração em runners Linux AMD64 e ARM64. Somente após ambos passarem, o workflow **Publish CasaOS image** publica `latest` e uma tag imutável `sha-<commit>` no GHCR usando o `GITHUB_TOKEN` do próprio repositório. Não é necessário cadastrar token pessoal nos secrets.

**Na primeira publicação, o pacote GHCR deve ficar público** para o CasaOS baixá-lo sem login. Se estiver privado, abra o pacote `baseguard` no perfil do GitHub → **Package settings → Change visibility → Public**. A visibilidade do repositório, sozinha, não garante a visibilidade do pacote. Consulte o resultado do workflow antes de tentar instalar.

### Alternativa: construir no próprio servidor

Requisitos: Linux de 64 bits, Docker com Compose v2 e acesso de rede aos bancos. Esta opção usa `compose.yaml` e a imagem `baseguard:local`, sem publicação externa.

Na pasta do projeto, no servidor:

```bash
cp .env.example .env
# Ajuste os caminhos e a porta no .env se necessário.

sudo install -d -m 0700 -o 10001 -g 10001 /DATA/AppData/baseguard/data
sudo install -d -m 0700 -o 10001 -g 10001 /DATA/AppData/baseguard/secrets
sudo install -d -m 0700 -o 10001 -g 10001 /DATA/Backups/baseguard

docker compose build
docker compose run --rm --no-deps baseguard init-destination /backups Principal
docker compose up -d
docker compose logs baseguard
```

Os exemplos usam os caminhos padrão do `.env.example`; se alterá-los, crie os diretórios correspondentes. A porta padrão é **8080**. Abra `http://IP-DO-SERVIDOR:8080`, entre como **admin** e use a senha inicial aleatória exibida uma única vez nos logs. Troque-a em **Configurações**.

O comando `init-destination` cria um marcador no destino e recusa sobrescrever um marcador existente. Execute-o apenas na configuração inicial, com o disco/NAS realmente montado. Depois de reinícios, a aplicação exige esse marcador; não cria automaticamente a pasta raiz de um destino ausente.

Escolha uma forma de iniciar o serviço: Compose no terminal ou importação no CasaOS, evitando duas instâncias. O bloqueio do diretório de dados impede dois processos de operar o mesmo SQLite. O Compose local executa diretamente com usuário `10001:10001`, por isso exige preparar os diretórios antes de iniciar.

Se preferir definir a senha inicial, monte um arquivo como segredo e configure `BASEGUARD_ADMIN_PASSWORD_FILE=/caminho/do/segredo`. Também existe `BASEGUARD_ADMIN_PASSWORD`. Esses valores são usados somente na criação do administrador; não redefinem uma conta existente. Use 12–72 bytes. A opção de arquivo evita manter a senha diretamente no ambiente do container.

### Discos externos, NAS e certificados

Monte discos e compartilhamentos **no Linux** antes de iniciar o container. Para NAS/USB, configure também a dependência de montagem no boot do host. O marcador reduz o risco de escrever no diretório de montagem vazio, mas não substitui a administração dessas montagens. Não copie o marcador para a pasta subjacente sem o disco montado.

Adicione um bind mount e o destino correspondente no Compose, por exemplo:

```yaml
environment:
  BASEGUARD_DESTINATIONS: '{"Principal":"/backups","NAS":"/nas"}'
volumes:
  - type: bind
    source: /mnt/nas/baseguard
    target: /nas
    bind:
      create_host_path: false
```

Prepare as permissões do diretório, execute `docker compose run --rm --no-deps baseguard init-destination /nas NAS` e recrie o container. O painel permite escolher o destino e uma subpasta, sempre confinada ao volume. A remoção de retenção também respeita o destino original de cada arquivo; se ele estiver indisponível, o histórico informa retenção pendente.

Para TLS com CA própria, monte o certificado como somente leitura (ex.: `/caminho/ca.pem:/certs/ca.pem:ro`) e informe `/certs/ca.pem` no cadastro. `TLS obrigatório` cifra a conexão sem validar a identidade. `TLS com validação` valida certificado e host; sem CA personalizada, usa as autoridades do sistema da imagem.

`localhost` no cadastro aponta para o próprio container. Para bancos em outros containers, conecte o BaseGuard à rede Docker correspondente e use o nome do serviço, ou utilize um endereço do host acessível ao container. Para bancos na internet, use TLS com validação.

## Armazenamento e recuperação

```text
/data/baseguard.db               configurações, histórico, fila e sessões
/data/baseguard.db-wal           arquivo auxiliar enquanto SQLite está ativo
/data/baseguard.db-shm           arquivo auxiliar enquanto SQLite está ativo
/secrets/master.key             chave para decifrar as credenciais
/backups/.baseguard-destination  identificação do destino montado
/backups/<subpasta>/db-<id>/      dumps, com data UTC e ID da execução
```

Mantenha o **SQLite em disco local**, mesmo quando os dumps estiverem em um NAS. Para copiar a configuração com segurança, pare o serviço e copie **todo o diretório `/data` e a chave de `/secrets`**, preservando permissões. Não copie apenas o `.db` de uma instância em execução, pois transações podem estar no WAL. Depois inicie o serviço novamente. Em outro servidor, restaure esses diretórios, os destinos e seus arquivos antes de iniciar. Se existir um `.db` sem a chave correspondente, a aplicação recusa criar uma chave substituta.

O cadastro removido preserva os dumps e o histórico. Enquanto houver um backup na fila ou em andamento, o cadastro não pode ser removido. Pausar o agendamento não cancela trabalhos já enfileirados. Backups enfileirados usam a configuração vigente quando começam.

Após reinício, execuções em andamento são marcadas como interrompidas e os `.partial` conhecidos são removidos quando o destino está acessível. Agendamentos vencidos geram no máximo uma execução de recuperação por banco; os próximos horários são calculados a partir do retorno do serviço. Se houve interrupção exatamente entre a renomeação e a confirmação no SQLite, pode sobrar um arquivo final não confirmado: ele fica fora de downloads e retenção automática, identificado pelo caminho registrado no histórico do SQLite. Inspecione-o antes de removê-lo manualmente. Falhas não apagam backups anteriores. A retenção pendente é tentada novamente após um próximo backup bem-sucedido.

### Restaurar um backup

Restaure primeiro em um banco vazio e descartável. O BaseGuard não executa restauração pelo painel e não inclui usuários globais, roles, tablespaces ou configuração do servidor.

PostgreSQL, com `pg_restore` compatível com o cliente usado no dump:

```bash
createdb -h HOST -U USUARIO banco_restaurado
pg_restore --exit-on-error --no-owner --no-privileges \
  -h HOST -U USUARIO -d banco_restaurado /caminho/backup.dump
```

As opções acima ignoram proprietários e ACLs durante a recuperação. Se precisa preservá-los, recrie previamente as roles necessárias e omita essas opções. Instale também as extensões exigidas pelo banco.

MySQL, com um cliente da mesma série e um banco vazio já criado:

```bash
set -o pipefail
gzip -dc /caminho/backup.sql.gz | mysql --defaults-extra-file=/caminho/restore.cnf banco_restaurado
```

O arquivo `restore.cnf` deve conter a conexão na seção `[client]`, com permissões `0600`. Não passe a senha na linha de comando. Dumps incluem definições de rotinas, eventos e triggers; a restauração pode exigir os usuários `DEFINER` originais e privilégios específicos. A checagem SHA-256 detecta alteração do arquivo; confirmar recuperação exige restaurar e conferir dados e objetos.

## Configuração do processo

| Variável | Padrão | Uso |
|---|---|---|
| `BASEGUARD_LISTEN` | `:8080` | Endereço HTTP dentro do container |
| `BASEGUARD_DATA_DIR` | `/data` | SQLite e bloqueio de instância |
| `BASEGUARD_KEY_FILE` | `/secrets/master.key` | Chave de 32 bytes gerada no primeiro início |
| `BASEGUARD_DESTINATIONS` | `{"Principal":"/backups"}` | Mapa JSON nome → caminho absoluto |
| `BASEGUARD_ADMIN_PASSWORD_FILE` | vazio | Segredo de inicialização do administrador |
| `BASEGUARD_ADMIN_PASSWORD` | aleatório | Alternativa de senha inicial |
| `BASEGUARD_SECURE_COOKIES` | `false` | Ative quando o acesso do navegador usar HTTPS |
| `BASEGUARD_INIT_LOCAL_DESTINATION` | `false` (`true` no Compose CasaOS) | Inicializa apenas o destino local padrão em uma instalação nova; nunca recria marcador de uma instância existente |

O painel foi pensado para rede privada/VPN. Caso use HTTPS em um proxy, preserve o cabeçalho `Host`, permita downloads longos e ative cookies seguros. O container não precisa de Docker socket, modo privilegiado ou servidor de banco adicional. `/healthz` verifica a disponibilidade do processo/SQLite; falhas de backup aparecem no histórico e nos logs, não alteram esse healthcheck.

## Desenvolvimento e testes

Go 1.26 ou superior; o Dockerfile fixa a ferramenta de build em Go 1.27.1. HTMX 2.0.10 está incluído localmente com sua licença. Não há npm, CDN ou etapa de frontend.

```bash
go test ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/baseguard ./cmd/baseguard
```

Para executar fora do container, defina diretórios absolutos locais em `BASEGUARD_DATA_DIR`, `BASEGUARD_KEY_FILE` e `BASEGUARD_DESTINATIONS`. Instale `psql`, `pg_dump`, `pg_restore`, `mysql` e `mysqldump` no PATH. `go run ./cmd/baseguard` inicia o painel. No container, esses clientes já estão incluídos.

Testes de integração **somente com bancos descartáveis**:

```bash
docker compose build
docker compose -f compose.test.yaml up --build --abort-on-container-exit --exit-code-from tests
docker compose -f compose.test.yaml down -v
```

A suíte cria serviços isolados sem portas publicadas, gera tabelas, dados, views, rotinas, triggers e evento MySQL, produz dumps usando o executor real e restaura em outros bancos. Também rejeita credenciais erradas e tabelas MyISAM. O processo de teste sai com erro se dados/objetos não forem recuperados. O comando `down -v` aplica-se apenas ao projeto descartável `baseguard-integration`.

Os testes locais cobrem autenticação, CSRF, persistência, criptografia, deduplicação, recuperação, retenção, falha de escrita, timeout, proteção de caminhos e downloads. A suíte de integração é ignorada no `go test` comum e habilitada apenas pelo Compose de testes.

Para medir o ARM64, use um banco representativo, acompanhe `docker stats --no-stream` antes/durante o dump e registre duração/tamanho pelo histórico. Meça também o tempo de recuperação. Não há limite de RAM artificial no Compose: o resultado real dependerá do cliente, banco, disco e compressão. Cada dump é transmitido diretamente ao arquivo e apenas um roda por vez.

## Limites desta versão

Backups lógicos completos de bancos individuais. Não inclui backup físico/incremental, recuperação ponto a ponto, restauração pela interface, múltiplos usuários, envio para nuvem, notificações externas ou criptografia dos arquivos de dump. A criptografia aplicada pelo BaseGuard protege as credenciais armazenadas no SQLite; proteja os próprios volumes e arquivos com os controles do servidor.

O histórico completo permanece no SQLite, embora o painel mostre apenas as últimas 200 execuções. Antes de usar em produção, rode a suíte de integração na arquitetura alvo e faça uma restauração dos seus próprios bancos em destino descartável.
