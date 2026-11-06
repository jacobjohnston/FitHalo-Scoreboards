// A FitHalo Scoreboard on Azure's free allowances: a container app that scales to zero, and a Cosmos DB
// for NoSQL account on its free tier. Deploy it with deploy-azure.sh from Azure Cloud Shell.

@description('Where the board runs.')
param location string = resourceGroup().location

@description('The container app\'s name, which is also the start of its address.')
param appName string = 'fithalo-scoreboard'

@description('The board\'s image. A release\'s copy of this template names the image by digest.')
param image string = 'ghcr.io/jacobjohnston/fithalo-scoreboards:latest'

@description('The code that creates the board\'s admin at /admin/setup. deploy-azure.sh makes one.')
@secure()
@minLength(16)
param setupCode string

@description('A secret mixed into password hashes, kept apart from the data. Never change it once set.')
@secure()
param pepper string = newGuid()

@description('Use Cosmos DB\'s free tier, of which a subscription has one.')
param freeTier bool = true

@description('Where to send an alert if the group\'s spending passes one dollar in a month.')
param budgetEmail string = ''

@description('The first day of the budget\'s first month.')
param budgetStart string = '${utcNow('yyyy-MM')}-01'

// accountName is unique to the resource group, as Cosmos DB account names are global.
var accountName = 'scoreboard${uniqueString(resourceGroup().id)}'

@description('The Cosmos DB for NoSQL account, on the free tier unless freeTier is false.')
resource account 'Microsoft.DocumentDB/databaseAccounts@2024-05-15' = {
  name: accountName
  location: location
  kind: 'GlobalDocumentDB'
  properties: {
    databaseAccountOfferType: 'Standard'
    enableFreeTier: freeTier
    minimalTlsVersion: 'Tls12'
    consistencyPolicy: {
      defaultConsistencyLevel: 'Strong'
    }
    locations: [
      {
        locationName: location
        failoverPriority: 0
        isZoneRedundant: false
      }
    ]
  }
}

@description('The database, with 400 RU/s shared throughput, inside the free tier\'s 1000.')
resource database 'Microsoft.DocumentDB/databaseAccounts/sqlDatabases@2024-05-15' = {
  parent: account
  name: 'scoreboard'
  properties: {
    resource: {
      id: 'scoreboard'
    }
    options: {
      throughput: 400
    }
  }
}

@description('The container that holds the board, partitioned by its c key.')
resource container 'Microsoft.DocumentDB/databaseAccounts/sqlDatabases/containers@2024-05-15' = {
  parent: database
  name: 'board'
  properties: {
    resource: {
      id: 'board'
      partitionKey: {
        paths: [
          '/c'
        ]
        kind: 'Hash'
      }
    }
  }
}

@description('The Container Apps environment the board runs in.')
resource environment 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: '${appName}-environment'
  location: location
  properties: {}
}

@description('The board: one replica at most, scaled to zero when idle.')
resource app 'Microsoft.App/containerApps@2024-03-01' = {
  name: appName
  location: location
  dependsOn: [
    container
  ]
  properties: {
    managedEnvironmentId: environment.id
    configuration: {
      ingress: {
        external: true
        targetPort: 8080
        transport: 'auto'
        allowInsecure: false
      }
      secrets: [
        {
          name: 'cosmos-key'
          value: account.listKeys().primaryMasterKey
        }
        {
          name: 'pepper'
          value: pepper
        }
        {
          name: 'setup-code'
          value: setupCode
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'scoreboard'
          image: image
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
          env: [
            { name: 'SCOREBOARD_STORE', value: 'cosmos' }
            { name: 'SCOREBOARD_COSMOS_ENDPOINT', value: account.properties.documentEndpoint }
            { name: 'SCOREBOARD_COSMOS_KEY', secretRef: 'cosmos-key' }
            { name: 'SCOREBOARD_COSMOS_DATABASE', value: 'scoreboard' }
            { name: 'SCOREBOARD_COSMOS_CONTAINER', value: 'board' }
            { name: 'SCOREBOARD_PEPPER', secretRef: 'pepper' }
            { name: 'SCOREBOARD_SETUP_CODE', secretRef: 'setup-code' }
            { name: 'SCOREBOARD_TLS', value: 'proxy' }
            { name: 'SCOREBOARD_LISTEN', value: '0.0.0.0:8080' }
            { name: 'SCOREBOARD_PUBLIC_URL', value: 'https://${appName}.${environment.properties.defaultDomain}' }
          ]
        }
      ]
      scale: {
        minReplicas: 0
        maxReplicas: 1
        rules: [
          {
            name: 'http'
            http: {
              metadata: {
                concurrentRequests: '50'
              }
            }
          }
        ]
      }
    }
  }
}

@description('A one-dollar monthly cost budget, made only when budgetEmail is given.')
resource budget 'Microsoft.Consumption/budgets@2023-05-01' = if (!empty(budgetEmail)) {
  name: '${appName}-one-dollar'
  properties: {
    category: 'Cost'
    amount: 1
    timeGrain: 'Monthly'
    timePeriod: {
      startDate: budgetStart
    }
    notifications: {
      actual: {
        enabled: true
        operator: 'GreaterThan'
        threshold: 100
        thresholdType: 'Actual'
        contactEmails: [
          budgetEmail
        ]
      }
    }
  }
}

@description('The board\'s address.')
output boardUrl string = 'https://${app.properties.configuration.ingress.fqdn}'
