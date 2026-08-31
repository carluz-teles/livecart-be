// Aplica as migrations usando exatamente o mesmo runner do boot da API.
//
// Existe para o desenvolvimento local poder migrar sem subir o servidor
// inteiro, que exige Clerk, S3 e o resto da configuração. Chama
// database.RunMigrations — a MESMA função que cmd/http-server/main.go usa —
// porque aplicar o SQL à mão divergiria do que a produção faz, e a divergência
// só apareceria quando alguém precisasse confiar no resultado.
package main

import (
	"fmt"
	"os"

	"livecart/apps/api/lib/database"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL é obrigatório")
		os.Exit(1)
	}
	caminho := os.Getenv("MIGRATIONS_PATH")
	if caminho == "" {
		caminho = "apps/api/db/migrations"
	}
	if err := database.RunMigrations(url, caminho); err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("migrations aplicadas")
}
